package tests

import (
	"testing"

	"github.com/containerd/continuity/fs/fstest"
	"github.com/docker/buildx/tests/workers"
	"github.com/moby/buildkit/util/testutil/integration"
)

func init() {
	workers.InitDockerWorker()
	workers.InitContainerWorker()
}

func TestIntegration(t *testing.T) {
	var tests []func(t *testing.T, sb integration.Sandbox)
	tests = append(tests, buildTests...)
	testIntegration(t, tests...)
}

func testIntegration(t *testing.T, funcs ...func(t *testing.T, sb integration.Sandbox)) {
	mirroredImages := integration.OfficialImages("busybox:latest", "alpine:latest")
	mirroredImages["moby/buildkit:latest"] = "docker.io/moby/buildkit:latest"
	mirrors := integration.WithMirroredImages(mirroredImages)

	tests := integration.TestFuncs(funcs...)
	integration.Run(t, tests, mirrors)
}

func tmpdir(t *testing.T, appliers ...fstest.Applier) (string, error) {
	tmpdir := t.TempDir()
	if err := fstest.Apply(appliers...).Apply(tmpdir); err != nil {
		return "", err
	}
	return tmpdir, nil
}
