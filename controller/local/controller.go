package local

import (
	"context"
	"io"
	"sync/atomic"

	"github.com/docker/buildx/build"
	cbuild "github.com/docker/buildx/controller/build"
	"github.com/docker/buildx/controller/control"
	controllerapi "github.com/docker/buildx/controller/pb"
	"github.com/docker/buildx/controller/processes"
	"github.com/docker/buildx/util/ioset"
	"github.com/docker/buildx/util/progress"
	"github.com/docker/cli/cli/command"
	"github.com/moby/buildkit/client"
	"github.com/pkg/errors"
)

func NewLocalBuildxController(ctx context.Context, dockerCli command.Cli) control.BuildxController {
	return &localController{
		dockerCli: dockerCli,
		ref:       "local",
		processes: processes.NewManager(),
	}
}

type localController struct {
	dockerCli command.Cli
	ref       string
	resultCtx *build.ResultContext
	processes *processes.Manager

	buildOnGoing atomic.Bool
}

func (b *localController) Build(ctx context.Context, options controllerapi.BuildOptions, in io.ReadCloser, progress progress.Writer) (string, *client.SolveResponse, error) {
	if !b.buildOnGoing.CompareAndSwap(false, true) {
		return "", nil, errors.New("build ongoing")
	}
	defer b.buildOnGoing.Store(false)

	resp, res, err := cbuild.RunBuild(ctx, b.dockerCli, options, in, progress)
	if err != nil {
		return "", nil, err
	}
	b.resultCtx = res
	return b.ref, resp, nil
}

func (b *localController) ListProcesses(ctx context.Context, ref string) (infos []*controllerapi.ProcessInfo, retErr error) {
	if ref != b.ref {
		return nil, errors.Errorf("unknown ref %q", ref)
	}
	return b.processes.ListProcesses(), nil
}

func (b *localController) DisconnectProcess(ctx context.Context, ref, pid string) error {
	if ref != b.ref {
		return errors.Errorf("unknown ref %q", ref)
	}
	return b.processes.DeleteProcess(pid)
}

func (b *localController) cancelRunningProcesses() {
	b.processes.CancelRunningProcesses()
}

func (b *localController) Invoke(ctx context.Context, ref string, pid string, cfg controllerapi.InvokeConfig, ioIn io.ReadCloser, ioOut io.WriteCloser, ioErr io.WriteCloser) error {
	if ref != b.ref {
		return errors.Errorf("unknown ref %q", ref)
	}

	proc, ok := b.processes.Get(pid)
	if !ok {
		// Start a new process.
		if b.resultCtx == nil {
			return errors.New("no build result is registered")
		}
		var err error
		proc, err = b.processes.StartProcess(pid, b.resultCtx, &cfg)
		if err != nil {
			return err
		}
	}

	// Attach containerIn to this process
	ioCancelledCh := make(chan struct{})
	proc.ForwardIO(&ioset.In{Stdin: ioIn, Stdout: ioOut, Stderr: ioErr}, func() { close(ioCancelledCh) })

	select {
	case <-ioCancelledCh:
		return errors.Errorf("io cancelled")
	case err := <-proc.Done():
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *localController) Kill(context.Context) error {
	b.cancelRunningProcesses()
	return nil
}

func (b *localController) Close() error {
	b.cancelRunningProcesses()
	// TODO: cancel ongoing builds?
	return nil
}

func (b *localController) List(ctx context.Context) (res []string, _ error) {
	return []string{b.ref}, nil
}

func (b *localController) Disconnect(ctx context.Context, key string) error {
	b.cancelRunningProcesses()
	return nil
}
