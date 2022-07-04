package commands

import (
	"context"
	"os"

	"github.com/docker/buildx/build"
	"github.com/docker/buildx/driver"
	remoteutil "github.com/docker/buildx/driver/remote/util"
	"github.com/docker/buildx/store"
	"github.com/docker/buildx/store/storeutil"
	"github.com/docker/buildx/util/driverloader"
	"github.com/docker/buildx/util/platformutil"
	"github.com/docker/buildx/util/progress"
	"github.com/docker/cli/cli/command"
	dopts "github.com/docker/cli/opts"
	dockerclient "github.com/docker/docker/client"
	"github.com/moby/buildkit/util/grpcerrors"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
)

// validateEndpoint validates that endpoint is either a context or a docker host
func validateEndpoint(dockerCli command.Cli, ep string) (string, error) {
	de, err := storeutil.GetDockerEndpoint(dockerCli, ep)
	if err == nil && de != "" {
		if ep == "default" {
			return de, nil
		}
		return ep, nil
	}
	h, err := dopts.ParseHost(true, ep)
	if err != nil {
		return "", errors.Wrapf(err, "failed to parse endpoint %s", ep)
	}
	return h, nil
}

// validateBuildkitEndpoint validates that endpoint is a valid buildkit host
func validateBuildkitEndpoint(ep string) (string, error) {
	if err := remoteutil.IsValidEndpoint(ep); err != nil {
		return "", err
	}
	return ep, nil
}

func dockerAPI(dockerCli command.Cli) *api {
	return &api{dockerCli: dockerCli}
}

type api struct {
	dockerCli command.Cli
}

func (a *api) DockerAPI(name string) (dockerclient.APIClient, error) {
	if name == "" {
		name = a.dockerCli.CurrentContext()
	}
	return driverloader.ClientForEndpoint(a.dockerCli, name)
}

func loadNodeGroupData(ctx context.Context, dockerCli command.Cli, ngi *nginfo) error {
	eg, _ := errgroup.WithContext(ctx)

	dis, err := driverloader.DriversForNodeGroup(ctx, dockerCli, ngi.ng, "")
	if err != nil {
		return err
	}
	ngi.drivers = make([]dinfo, len(dis))
	for i, di := range dis {
		d := di
		ngi.drivers[i].di = &d
		func(d *dinfo) {
			eg.Go(func() error {
				if err := loadInfoData(ctx, d); err != nil {
					d.err = err
				}
				return nil
			})
		}(&ngi.drivers[i])
	}

	if eg.Wait(); err != nil {
		return err
	}

	kubernetesDriverCount := 0

	for _, di := range ngi.drivers {
		if di.info != nil && len(di.info.DynamicNodes) > 0 {
			kubernetesDriverCount++
		}
	}

	isAllKubernetesDrivers := len(ngi.drivers) == kubernetesDriverCount

	if isAllKubernetesDrivers {
		var drivers []dinfo
		var dynamicNodes []store.Node

		for _, di := range ngi.drivers {
			// dynamic nodes are used in Kubernetes driver.
			// Kubernetes pods are dynamically mapped to BuildKit Nodes.
			if di.info != nil && len(di.info.DynamicNodes) > 0 {
				for i := 0; i < len(di.info.DynamicNodes); i++ {
					// all []dinfo share *build.DriverInfo and *driver.Info
					diClone := di
					if pl := di.info.DynamicNodes[i].Platforms; len(pl) > 0 {
						diClone.platforms = pl
					}
					drivers = append(drivers, di)
				}
				dynamicNodes = append(dynamicNodes, di.info.DynamicNodes...)
			}
		}

		// not append (remove the static nodes in the store)
		ngi.ng.Nodes = dynamicNodes
		ngi.drivers = drivers
		ngi.ng.Dynamic = true
	}

	return nil
}

func hasNodeGroup(list []*nginfo, ngi *nginfo) bool {
	for _, l := range list {
		if ngi.ng.Name == l.ng.Name {
			return true
		}
	}
	return false
}

type nginfo struct {
	ng      *store.NodeGroup
	drivers []dinfo
	err     error
}

// inactive checks if all nodes are inactive for this builder
func (n *nginfo) inactive() bool {
	for idx := range n.ng.Nodes {
		d := n.drivers[idx]
		if d.info != nil && d.info.Status == driver.Running {
			return false
		}
	}
	return true
}

func boot(ctx context.Context, ngi *nginfo) (bool, error) {
	toBoot := make([]int, 0, len(ngi.drivers))
	for i, d := range ngi.drivers {
		if d.err != nil || d.di.Err != nil || d.di.Driver == nil || d.info == nil {
			continue
		}
		if d.info.Status != driver.Running {
			toBoot = append(toBoot, i)
		}
	}
	if len(toBoot) == 0 {
		return false, nil
	}

	printer := progress.NewPrinter(context.TODO(), os.Stderr, os.Stderr, "auto")

	baseCtx := ctx
	eg, _ := errgroup.WithContext(ctx)
	for _, idx := range toBoot {
		func(idx int) {
			eg.Go(func() error {
				pw := progress.WithPrefix(printer, ngi.ng.Nodes[idx].Name, len(toBoot) > 1)
				_, err := driver.Boot(ctx, baseCtx, ngi.drivers[idx].di.Driver, pw)
				if err != nil {
					ngi.drivers[idx].err = err
				}
				return nil
			})
		}(idx)
	}

	err := eg.Wait()
	err1 := printer.Wait()
	if err == nil {
		err = err1
	}

	return true, err
}

type dinfo struct {
	di        *build.DriverInfo
	info      *driver.Info
	platforms []specs.Platform
	version   string
	err       error
}

func loadInfoData(ctx context.Context, d *dinfo) error {
	if d.di.Driver == nil {
		return nil
	}
	info, err := d.di.Driver.Info(ctx)
	if err != nil {
		return err
	}
	d.info = info
	if info.Status == driver.Running {
		c, err := d.di.Driver.Client(ctx)
		if err != nil {
			return err
		}
		workers, err := c.ListWorkers(ctx)
		if err != nil {
			return errors.Wrap(err, "listing workers")
		}
		for _, w := range workers {
			d.platforms = append(d.platforms, w.Platforms...)
		}
		d.platforms = platformutil.Dedupe(d.platforms)
		inf, err := c.Info(ctx)
		if err != nil {
			if st, ok := grpcerrors.AsGRPCStatus(err); ok && st.Code() == codes.Unimplemented {
				d.version, err = d.di.Driver.Version(ctx)
				if err != nil {
					return errors.Wrap(err, "getting version")
				}
			}
		} else {
			d.version = inf.BuildkitVersion.Version
		}
	}
	return nil
}
