package workers

import (
	"context"
	"os"
	"os/exec"

	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/util/testutil/integration"
	"github.com/pkg/errors"
)

func InitContainerWorker() {
	integration.Register(&containerWorker{})
}

type containerWorker struct{}

func (w *containerWorker) Name() string {
	return "container"
}

func (w *containerWorker) Rootless() bool {
	return false
}

func (w *containerWorker) New(ctx context.Context, cfg *integration.BackendConfig) (integration.Backend, func() error, error) {
	config, err := os.CreateTemp("", "buildkit.config.toml")
	if err != nil {
		return nil, nil, err
	}

	name := "container" + identity.NewID()

	cmd := exec.Command("buildx", "create", "--bootstrap", "--name="+name, "--driver=docker-container", "--config="+config.Name())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		return nil, nil, errors.Wrapf(err, "failed to create buildx instance %s", name)
	}

	close := func() error {
		cmd := exec.Command("buildx", "rm", "-f", name)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if err1 := os.Remove(config.Name()); err1 != nil && err == nil {
			return errors.Wrapf(err, "failed to remove config for buildx instance %s", name)
		}
		return errors.Wrapf(err, "failed to remove buildx instance %s", name)
	}

	return &dummyBackend{name: name}, close, nil
}

type dummyBackend struct {
	name string
}

func (s *dummyBackend) Address() string {
	return s.name
}

func (s *dummyBackend) ContainerdAddress() string {
	return ""
}

func (s *dummyBackend) Snapshotter() string {
	return ""
}

func (s *dummyBackend) Rootless() bool {
	return false
}
