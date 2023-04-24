package tests

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/moby/buildkit/util/testutil/integration"
	"github.com/stretchr/testify/require"
)

func inspect(sb integration.Sandbox, args ...string) (string, error) {
	args = append([]string{"inspect", "--builder=" + sb.Address()}, args...)
	cmd := exec.Command("buildx", args...)

	out, err := cmd.CombinedOutput()
	return string(out), err
}

func testInspect(t *testing.T, sb integration.Sandbox) {
	out, err := inspect(sb)
	require.NoError(t, err, string(out))

	var name string
	for _, line := range strings.Split(out, "\n") {
		var ok bool
		name, ok = strings.CutPrefix(line, "Name:")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		break
	}
	require.Equal(t, sb.Address(), name)
}
