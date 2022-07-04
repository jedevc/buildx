package driverloader

import (
	"context"
	"net/url"
	"os"
	"strings"

	"github.com/docker/buildx/build"
	"github.com/docker/buildx/driver"
	ctxkube "github.com/docker/buildx/driver/kubernetes/context"
	"github.com/docker/buildx/store"
	"github.com/docker/buildx/store/storeutil"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/context/docker"
	ctxstore "github.com/docker/cli/cli/context/store"
	dockerclient "github.com/docker/docker/client"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/tools/clientcmd"
)

// driversForNodeGroup returns drivers for a nodegroup instance
func DriversForNodeGroup(ctx context.Context, dockerCli command.Cli, ng *store.NodeGroup, contextPathHash string) ([]build.DriverInfo, error) {
	eg, _ := errgroup.WithContext(ctx)

	dis := make([]build.DriverInfo, len(ng.Nodes))

	var f driver.Factory
	if ng.Driver != "" {
		f = driver.GetFactory(ng.Driver, true)
		if f == nil {
			return nil, errors.Errorf("failed to find driver %q", f)
		}
	} else {
		ep := ng.Nodes[0].Endpoint
		dockerapi, err := ClientForEndpoint(dockerCli, ep)
		if err != nil {
			return nil, err
		}
		f, err = driver.GetDefaultFactory(ctx, ep, dockerapi, false)
		if err != nil {
			return nil, err
		}
		ng.Driver = f.Name()
	}
	imageopt, err := storeutil.GetImageConfig(dockerCli, ng)
	if err != nil {
		return nil, err
	}

	for i, n := range ng.Nodes {
		func(i int, n store.Node) {
			eg.Go(func() error {
				di := build.DriverInfo{
					Name:        n.Name,
					Platform:    n.Platforms,
					ProxyConfig: storeutil.GetProxyConfig(dockerCli),
				}
				defer func() {
					dis[i] = di
				}()

				dockerapi, err := ClientForEndpoint(dockerCli, n.Endpoint)
				if err != nil {
					di.Err = err
					return nil
				}
				// TODO: replace the following line with dockerclient.WithAPIVersionNegotiation option in clientForEndpoint
				dockerapi.NegotiateAPIVersion(ctx)

				contextStore := dockerCli.ContextStore()

				var kcc driver.KubeClientConfig
				kcc, err = configFromContext(n.Endpoint, contextStore)
				if err != nil {
					// err is returned if n.Endpoint is non-context name like "unix:///var/run/docker.sock".
					// try again with name="default".
					// FIXME: n should retain real context name.
					kcc, err = configFromContext("default", contextStore)
					if err != nil {
						logrus.Error(err)
					}
				}

				tryToUseKubeConfigInCluster := false
				if kcc == nil {
					tryToUseKubeConfigInCluster = true
				} else {
					if _, err := kcc.ClientConfig(); err != nil {
						tryToUseKubeConfigInCluster = true
					}
				}
				if tryToUseKubeConfigInCluster {
					kccInCluster := driver.KubeClientConfigInCluster{}
					if _, err := kccInCluster.ClientConfig(); err == nil {
						logrus.Debug("using kube config in cluster")
						kcc = kccInCluster
					}
				}

				d, err := driver.GetDriver(ctx, "buildx_buildkit_"+n.Name, f, n.Endpoint, dockerapi, imageopt.Auth, kcc, n.Flags, n.Files, n.DriverOpts, n.Platforms, contextPathHash)
				if err != nil {
					di.Err = err
					return nil
				}
				di.Driver = d
				di.ImageOpt = imageopt
				return nil
			})
		}(i, n)
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	return dis, nil
}

func configFromContext(endpointName string, s ctxstore.Reader) (clientcmd.ClientConfig, error) {
	if strings.HasPrefix(endpointName, "kubernetes://") {
		u, _ := url.Parse(endpointName)
		if kubeconfig := u.Query().Get("kubeconfig"); kubeconfig != "" {
			_ = os.Setenv(clientcmd.RecommendedConfigPathEnvVar, kubeconfig)
		}
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		apiConfig, err := rules.Load()
		if err != nil {
			return nil, err
		}
		return clientcmd.NewDefaultClientConfig(*apiConfig, &clientcmd.ConfigOverrides{}), nil
	}
	return ctxkube.ConfigFromContext(endpointName, s)
}

// ClientForEndpoint returns a docker client for an endpoint
func ClientForEndpoint(dockerCli command.Cli, name string) (dockerclient.APIClient, error) {
	list, err := dockerCli.ContextStore().List()
	if err != nil {
		return nil, err
	}
	for _, l := range list {
		if l.Name == name {
			dep, ok := l.Endpoints["docker"]
			if !ok {
				return nil, errors.Errorf("context %q does not have a Docker endpoint", name)
			}
			epm, ok := dep.(docker.EndpointMeta)
			if !ok {
				return nil, errors.Errorf("endpoint %q is not of type EndpointMeta, %T", dep, dep)
			}
			ep, err := docker.WithTLSData(dockerCli.ContextStore(), name, epm)
			if err != nil {
				return nil, err
			}
			clientOpts, err := ep.ClientOpts()
			if err != nil {
				return nil, err
			}
			return dockerclient.NewClientWithOpts(clientOpts...)
		}
	}

	ep := docker.Endpoint{
		EndpointMeta: docker.EndpointMeta{
			Host: name,
		},
	}

	clientOpts, err := ep.ClientOpts()
	if err != nil {
		return nil, err
	}

	return dockerclient.NewClientWithOpts(clientOpts...)
}

func GetInstanceOrDefault(ctx context.Context, dockerCli command.Cli, instance, contextPathHash string) ([]build.DriverInfo, error) {
	var defaultOnly bool

	if instance == "default" && instance != dockerCli.CurrentContext() {
		return nil, errors.Errorf("use `docker --context=default buildx` to switch to default context")
	}
	if instance == "default" || instance == dockerCli.CurrentContext() {
		instance = ""
		defaultOnly = true
	}
	list, err := dockerCli.ContextStore().List()
	if err != nil {
		return nil, err
	}
	for _, l := range list {
		if l.Name == instance {
			return nil, errors.Errorf("use `docker --context=%s buildx` to switch to context %s", instance, instance)
		}
	}

	if instance != "" {
		return getInstanceByName(ctx, dockerCli, instance, contextPathHash)
	}
	return GetDefaultDrivers(ctx, dockerCli, defaultOnly, contextPathHash)
}

func getInstanceByName(ctx context.Context, dockerCli command.Cli, instance, contextPathHash string) ([]build.DriverInfo, error) {
	txn, release, err := storeutil.GetStore(dockerCli)
	if err != nil {
		return nil, err
	}
	defer release()

	ng, err := txn.NodeGroupByName(instance)
	if err != nil {
		return nil, err
	}
	return DriversForNodeGroup(ctx, dockerCli, ng, contextPathHash)
}

// GetDefaultDrivers returns drivers based on current cli config
func GetDefaultDrivers(ctx context.Context, dockerCli command.Cli, defaultOnly bool, contextPathHash string) ([]build.DriverInfo, error) {
	txn, release, err := storeutil.GetStore(dockerCli)
	if err != nil {
		return nil, err
	}
	defer release()

	if !defaultOnly {
		ng, err := storeutil.GetCurrentInstance(txn, dockerCli)
		if err != nil {
			return nil, err
		}

		if ng != nil {
			return DriversForNodeGroup(ctx, dockerCli, ng, contextPathHash)
		}
	}

	imageopt, err := storeutil.GetImageConfig(dockerCli, nil)
	if err != nil {
		return nil, err
	}

	d, err := driver.GetDriver(ctx, "buildx_buildkit_default", nil, "", dockerCli.Client(), imageopt.Auth, nil, nil, nil, nil, nil, contextPathHash)
	if err != nil {
		return nil, err
	}
	return []build.DriverInfo{
		{
			Name:        "default",
			Driver:      d,
			ImageOpt:    imageopt,
			ProxyConfig: storeutil.GetProxyConfig(dockerCli),
		},
	}, nil
}
