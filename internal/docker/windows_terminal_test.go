package docker

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type terminalWindowsDriver struct {
	windowsDriver
	requests []ExecRequest
	process  runtimes.Process
}

func (d *terminalWindowsDriver) Exec(_ context.Context, request ExecRequest) (runtimes.Process, error) {
	d.requests = append(d.requests, request)
	return d.process, nil
}

func TestNativeWindowsContainersTerminalStartsSDKExecWithConsoleAndShell(t *testing.T) {
	for _, shell := range []string{"", "cmd.exe", "powershell.exe"} {
		t.Run(shell, func(t *testing.T) {
			manager := nativeTestManager(t, &fakeRunner{})
			driver := &terminalWindowsDriver{process: &windowsSessionProcess{}}
			manager.windows.driver = driver

			process, err := manager.StartWindowsContainerTerminal(context.Background(), "synthetic-native", shell, false)
			if err != nil || process != driver.process {
				t.Fatalf("native terminal process = %v, %v", process, err)
			}
			if shell == "" {
				shell = "cmd.exe"
			}
			want := []ExecRequest{{
				ContainerID: "synthetic-native", Command: []string{shell},
				TTY: true, AttachStdin: true, AttachStdout: true, AttachStderr: true,
				ConsoleSize: [2]uint{32, 120},
			}}
			if !reflect.DeepEqual(driver.requests, want) {
				t.Fatalf("native SDK terminal requests = %+v, want %+v", driver.requests, want)
			}
		})
	}
}

func TestNativeWindowsContainersTerminalRejectsUnsupportedRequestsBeforeExec(t *testing.T) {
	for _, test := range []struct {
		name, id, shell string
		debug           bool
	}{
		{name: "debug", id: "synthetic-native", shell: "cmd.exe", debug: true},
		{name: "Linux shell", id: "synthetic-native", shell: "/bin/sh"},
		{name: "shell arguments", id: "synthetic-native", shell: "cmd.exe /c whoami"},
		{name: "unsafe ID", id: "--foreign", shell: "cmd.exe"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{}
			manager := nativeTestManager(t, runner)
			driver := &terminalWindowsDriver{}
			manager.windows.driver = driver
			if _, err := manager.StartWindowsContainerTerminal(context.Background(), test.id, test.shell, test.debug); err == nil {
				t.Fatal("unsupported native terminal request succeeded")
			}
			if len(driver.requests) != 0 || len(runner.commands) != 0 {
				t.Fatal("unsupported terminal request started an SDK exec or CLI helper")
			}
		})
	}
	manager := nativeTestManager(t, &fakeRunner{})
	if _, err := manager.ContainerTerminalCommand(context.Background(), "synthetic-native", "cmd.exe", false); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("native terminal retained its nerdctl/host-PTY command path: %v", err)
	}
}
