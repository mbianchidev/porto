//go:build darwin

package docker

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLaunchDaemonInstallerLoadsReplacesAndRemovesJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), EndpointLaunchDaemonLabel+".plist")
	loaded := false
	failBootstrap := false
	var calls []string
	installer := launchDaemonInstaller{path: path, launchctl: func(args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "print":
			if !loaded {
				return []byte("Could not find service"), errors.New("exit status 113")
			}
		case "bootstrap":
			if failBootstrap {
				return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
			}
			loaded = true
		case "bootout":
			loaded = false
		}
		return nil, nil
	}}
	service := "system/" + EndpointLaunchDaemonLabel

	if err := installer.install("/var/run/docker.sock", "/Users/example/Library/Application Support/porto/run/docker.sock"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("installed plist = %v, %v; want mode 0644", info, err)
	}
	if output, err := exec.Command("/usr/bin/plutil", "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil rejected the LaunchDaemon: %v: %s", err, output)
	}
	if err := installer.install("/var/run/docker.sock", "/tmp/porto.sock"); err != nil {
		t.Fatal(err)
	}
	if err := installer.remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plist remained after removal: %v", err)
	}
	want := []string{
		"print " + service, "bootstrap system " + path,
		"print " + service, "bootout " + service, "bootstrap system " + path,
		"print " + service, "bootout " + service,
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("launchctl calls = %q, want %q", calls, want)
	}

	failBootstrap = true
	if err := installer.install("/var/run/docker.sock", "/tmp/porto.sock"); err == nil || !strings.Contains(err.Error(), "Input/output error") {
		t.Fatalf("bootstrap failure = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plist remained after a failed bootstrap: %v", err)
	}
}

func TestDeactivateRemovesPersistedEndpointAfterReactivation(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "porto-persist-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	target := filepath.Join(dir, "porto.sock")
	listener, err := net.Listen("unix", target)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	loaded := false
	original := endpointLaunchDaemons
	endpointLaunchDaemons = launchDaemonInstaller{path: filepath.Join(dir, "job.plist"), launchctl: func(args ...string) ([]byte, error) {
		switch args[0] {
		case "print":
			if !loaded {
				return nil, errors.New("exit status 113")
			}
		case "bootstrap":
			loaded = true
		case "bootout":
			loaded = false
		}
		return nil, nil
	}}
	t.Cleanup(func() { endpointLaunchDaemons = original })
	canonical := filepath.Join(dir, "docker.sock")
	statePath := filepath.Join(dir, "state.json")

	if _, err := ActivateEndpoint(canonical, target, statePath, false); err != nil {
		t.Fatal(err)
	}
	if state, err := PersistEndpoint(statePath); err != nil || !state.Persistent || !EndpointPersisted() || !loaded {
		t.Fatalf("persist = %+v, %v; installed %t, loaded %t", state, err, EndpointPersisted(), loaded)
	}
	if err := os.Remove(canonical); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "other.sock"), canonical); err != nil {
		t.Fatal(err)
	}
	if state, err := ActivateEndpoint(canonical, target, statePath, true); err != nil || !state.Persistent {
		t.Fatalf("re-activation = %+v, %v; want the persistent job kept on record", state, err)
	}
	if err := DeactivateEndpoint(statePath); err != nil {
		t.Fatal(err)
	}
	if EndpointPersisted() || loaded {
		t.Fatalf("deactivation left the boot-time job installed %t, loaded %t", EndpointPersisted(), loaded)
	}
}
