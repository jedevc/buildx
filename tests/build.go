package tests

import (
	"os"
	"os/exec"
	"testing"

	"github.com/containerd/continuity/fs/fstest"
	"github.com/moby/buildkit/util/testutil/integration"
	"github.com/stretchr/testify/require"
)

func build(sb integration.Sandbox, args ...string) (string, error) {
	args = append([]string{"build", "--progress=quiet", "--builder=" + sb.Address()}, args...)
	cmd := exec.Command("buildx", args...)

	out, err := cmd.CombinedOutput()
	return string(out), err
}

func testBuild(t *testing.T, sb integration.Sandbox) {
	dockerfile := []byte(`
FROM busybox:latest
COPY foo /etc/foo
RUN cp /etc/foo /etc/bar

FROM scratch
COPY --from=0 /etc/bar /bar
`)
	dir, err := tmpdir(
		t,
		fstest.CreateFile("Dockerfile", dockerfile, 0600),
		fstest.CreateFile("foo", []byte("foo"), 0600),
	)
	require.NoError(t, err)

	out, err := build(sb, "--output=type=local,dest="+dir+"/result", dir)
	require.NoError(t, err, string(out))

	dt, err := os.ReadFile(dir + "/result/bar")
	require.NoError(t, err)
	require.Equal(t, "foo", string(dt))
}
