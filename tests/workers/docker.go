package workers

import (
	"context"
	"os/exec"
	"strings"

	"github.com/moby/buildkit/util/testutil/integration"
	"github.com/pkg/errors"
)

func InitDockerWorker() {
	integration.Register(&dockerWorker{})
}

type dockerWorker struct{}

func (w *dockerWorker) Name() string {
	return "docker"
}

func (w *dockerWorker) Rootless() bool {
	return false
}

func (w *dockerWorker) New(ctx context.Context, cfg *integration.BackendConfig) (integration.Backend, func() error, error) {
	// use moby for inspiration to apply cfg

	cmd := exec.Command("docker", "context", "inspect", "-f", "{{.Name}}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, nil, errors.Wrapf(err, "failed to get current context")
	}
	name := strings.TrimSpace(string(out))

	return &dummyBackend{name: name}, func() error { return nil }, nil
}
