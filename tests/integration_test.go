package tests

import (
	"testing"

	"github.com/containerd/continuity/fs/fstest"
	"github.com/docker/buildx/tests/workers"
	"github.com/moby/buildkit/util/testutil/integration"
)

func init() {
	workers.InitContainerWorker()
}

func TestIntegration(t *testing.T) {
	testIntegration(
		t,
		testBuild,
		testInspect,
	)
}

func testIntegration(t *testing.T, funcs ...func(t *testing.T, sb integration.Sandbox)) {
	mirroredImages := integration.OfficialImages("busybox:latest", "alpine:latest")
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
