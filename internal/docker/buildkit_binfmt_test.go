package docker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestLimaBinfmtProvisioningScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the provisioning script runs in a Linux guest")
	}
	for _, test := range []struct {
		name         string
		installed    string
		legacy       bool
		registration string
		omitted      string
		missingRule  bool
		failure      string
		wantPackage  string
		wantReload   bool
		wantError    string
	}{
		{name: "current Ubuntu", wantPackage: "qemu-user-binfmt", wantReload: true},
		{name: "older Ubuntu", legacy: true, wantPackage: "qemu-user-static", wantReload: true},
		{name: "current offline startup", installed: "current", registration: "ready"},
		{name: "older offline startup", installed: "legacy", registration: "ready"},
		{name: "dynamic QEMU is insufficient", installed: "dynamic", legacy: true, registration: "ready", wantPackage: "qemu-user-static"},
		{name: "restore registrations", installed: "current", wantReload: true},
		{name: "restore disabled handler", installed: "current", registration: "disabled", wantReload: true},
		{name: "restore fix-binary flag", installed: "current", registration: "no-F", wantReload: true},
		{name: "restore missing architecture", installed: "current", registration: "partial", wantReload: true},
		{name: "ARM64 package omits ARM32", installed: "current", omitted: "qemu-arm", registration: "ready", wantReload: true},
		{name: "older package omits i386", installed: "legacy", legacy: true, omitted: "qemu-i386", registration: "ready", wantReload: true},
		{name: "missing compatibility rule", installed: "current", omitted: "qemu-arm", missingRule: true, registration: "ready", wantError: "packaged binfmt definition for qemu-arm"},
		{name: "package index failure", failure: "update", wantError: "mock update failed"},
		{name: "package installation failure", failure: "install", wantPackage: "qemu-user-binfmt", wantError: "mock install failed"},
		{name: "registration service failure", installed: "current", failure: "reload", wantReload: true, wantError: "mock reload failed"},
		{name: "unusable handlers fail readiness", installed: "current", failure: "unusable", wantReload: true, wantError: "lack the F flag"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "guest with spaces")
			for _, directory := range []string{"bin", "binfmt", "definitions", "available", "packaged", "legacy", "overrides"} {
				if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			write := func(path, content string, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, path), []byte(content), mode); err != nil {
					t.Fatal(err)
				}
			}
			write("emulator", "#!/bin/sh\nexit 0\n", 0o700)
			interpreters := []string{"qemu-x86_64", "qemu-aarch64", "qemu-arm", "qemu-i386"}
			for _, name := range interpreters {
				definition := fmt.Sprintf("# Synthetic guest definition\n:%s:M:0:magic:mask:%s:F\n", name, filepath.Join(root, "emulator"))
				write("available/"+name+".conf", definition, 0o600)
				if test.installed != "" && name != test.omitted {
					write("definitions/"+name+".conf", definition, 0o600)
				}
				if name == test.omitted && !test.missingRule {
					directory := "packaged"
					if test.legacy {
						directory = "legacy"
					}
					write(directory+"/"+name+".conf", definition, 0o600)
				}
			}
			write("commands", "", 0o600)
			if test.installed != "" {
				write("installed", test.installed, 0o600)
			}
			if test.registration != "" {
				write("binfmt/status", "enabled\n", 0o600)
				for _, name := range interpreters {
					if name == test.omitted || (test.registration == "partial" && name == "qemu-aarch64") {
						continue
					}
					state, flags := "enabled", "POCF"
					if test.registration == "disabled" {
						state = "disabled"
					}
					if test.registration == "no-F" {
						flags = "POC"
					}
					write("binfmt/"+name, state+"\nflags: "+flags+"\n", 0o600)
				}
			}
			for _, tool := range []string{"dpkg-query", "apt-cache", "apt-get", "systemctl"} {
				write("bin/"+tool, binfmtMockTool, 0o700)
			}
			script := strings.NewReplacer(
				"/proc/sys/fs/binfmt_misc", shellJoin([]string{filepath.Join(root, "binfmt")}),
				"/usr/lib/binfmt.d", shellJoin([]string{filepath.Join(root, "definitions")}),
				"/usr/share/qemu/binfmt.d", shellJoin([]string{filepath.Join(root, "packaged")}),
				"/usr/share/doc/qemu-user-static", shellJoin([]string{filepath.Join(root, "legacy")}),
				"/etc/binfmt.d", shellJoin([]string{filepath.Join(root, "overrides")}),
			).Replace(limaBinfmtInstallCommand)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			output, err := (runtimes.ExecRunner{}).Run(ctx, runtimes.Command{
				Name: "sh",
				Args: []string{"-c", script},
				Env: []string{
					"PATH=" + filepath.Join(root, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
					"PORTO_BINFMT_TEST_ROOT=" + root,
					fmt.Sprintf("PORTO_BINFMT_TEST_LEGACY=%t", test.legacy),
					"PORTO_BINFMT_TEST_FAILURE=" + test.failure,
				},
			})
			if test.wantError != "" {
				if err == nil || !strings.Contains(string(output), test.wantError) {
					t.Fatalf("setup = %v, %s; want %q", err, output, test.wantError)
				}
			} else if err != nil {
				t.Fatalf("setup: %v: %s", err, output)
			}
			if test.omitted != "" && test.wantError == "" {
				target, err := os.Readlink(filepath.Join(root, "overrides", "porto-"+test.omitted+".conf"))
				directory := "packaged"
				if test.legacy {
					directory = "legacy"
				}
				if err != nil || target != filepath.Join(root, directory, test.omitted+".conf") {
					t.Fatalf("compatibility registration was not persisted: target=%q, error=%v", target, err)
				}
			}
			log, err := os.ReadFile(filepath.Join(root, "commands"))
			if err != nil {
				t.Fatal(err)
			}
			commands := string(log)
			if test.wantPackage == "" {
				if strings.Contains(commands, " install ") {
					t.Fatalf("unexpected package installation:\n%s", commands)
				}
			} else if !strings.Contains(commands, "install --yes --no-install-recommends "+test.wantPackage) {
				t.Fatalf("missing %s installation:\n%s", test.wantPackage, commands)
			}
			reloaded := strings.Contains(commands, "systemctl restart systemd-binfmt.service")
			if reloaded != test.wantReload {
				t.Fatalf("registration reload = %t, want %t:\n%s", reloaded, test.wantReload, commands)
			}
			if test.installed != "" && test.wantPackage == "" && strings.Contains(commands, "apt-") {
				t.Fatalf("configured engine required package/network access:\n%s", commands)
			}
		})
	}
}

const binfmtMockTool = `#!/bin/sh
set -eu
root="$PORTO_BINFMT_TEST_ROOT"
tool="${0##*/}"
case "$tool" in
  dpkg-query)
    [ -f "$root/installed" ] || exit 1
    installed="$(cat "$root/installed")"
    case "$*" in
      *qemu-user-static)
        [ "$installed" = legacy ] || exit 1
        printf 'install ok installed\n'
        ;;
      *qemu-user-binfmt)
        [ "$installed" != legacy ] || exit 1
        if [ "$installed" = current ]; then
          printf 'install ok installed qemu-user-static\n'
        else
          printf 'install ok installed\n'
        fi
        ;;
      *) exit 1 ;;
    esac
    ;;
  apt-cache)
    printf '%s\n' "$tool $*" >> "$root/commands"
    [ "$PORTO_BINFMT_TEST_LEGACY" = true ] || exit 100
    printf 'Package: qemu-user-static\n'
    ;;
  apt-get)
    printf '%s\n' "$tool $*" >> "$root/commands"
    case "$*" in
      *update) stage=update ;;
      *install*) stage=install ;;
      *) echo "unexpected apt invocation" >&2; exit 1 ;;
    esac
    if [ "$PORTO_BINFMT_TEST_FAILURE" = "$stage" ]; then
      echo "mock $stage failed" >&2
      exit 7
    fi
    if [ "$stage" = install ]; then
      [ "$DEBIAN_FRONTEND" = noninteractive ]
      [ "$NEEDRESTART_MODE" = l ]
      cp "$root"/available/*.conf "$root/definitions/"
    fi
    ;;
  systemctl)
    printf '%s\n' "$tool $*" >> "$root/commands"
    [ "$*" = "restart systemd-binfmt.service" ] || exit 1
    if [ "$PORTO_BINFMT_TEST_FAILURE" = reload ]; then
      echo "mock reload failed" >&2
      exit 7
    fi
    printf 'enabled\n' > "$root/binfmt/status"
    flags=POCF
    if [ "$PORTO_BINFMT_TEST_FAILURE" = unusable ]; then
      flags=POC
    fi
    for name in qemu-x86_64 qemu-aarch64 qemu-arm qemu-i386; do
      printf 'enabled\nflags: %s\n' "$flags" > "$root/binfmt/$name"
    done
    ;;
  *) exit 1 ;;
esac
`
