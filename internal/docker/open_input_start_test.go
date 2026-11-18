//go:build !windows

package docker

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenInputStartScriptWaitsForRunningOrReportsNerdctlErrors(t *testing.T) {
	bin := t.TempDir()
	for name, body := range map[string]string{
		"setsid": "#!/bin/sh\nexec \"$@\"\n",
		"nerdctl": `#!/bin/sh
case "$1" in
start)
  if [ -n "$FAKE_START_ERROR" ]; then echo "$FAKE_START_ERROR" >&2; exit 3; fi
  [ "$2 $3 $4" = "--attach --interactive demo" ] || exit 9
  touch "$FAKE_STATE"; sleep 1 ;;
inspect) if [ -f "$FAKE_STATE" ]; then echo running; else echo created; fi ;;
esac
`,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	run := func(startError string) (string, error) {
		command := exec.Command("sh", "-c", openInputStartScript, "porto-open-input", "demo")
		command.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "FAKE_STATE="+filepath.Join(t.TempDir(), "running"), "FAKE_START_ERROR="+startError)
		output, err := command.CombinedOutput()
		return strings.TrimSpace(string(output)), err
	}

	if output, err := run(""); err != nil {
		t.Fatalf("open-input start failed: %v: %s", err, output)
	}
	output, err := run("port is already allocated")
	if exitErr := (*exec.ExitError)(nil); err == nil || !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 || output != "port is already allocated" {
		t.Fatalf("failed start = %v, %q; want nerdctl's exit status and error", err, output)
	}
}
