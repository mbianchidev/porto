//go:build windows

package process

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const stillActive = 259

func TestKillTerminatesWindowsProcessTree(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := t.TempDir() + `\child.pid`
	command := NewCommand(context.Background(), "", executable, "-test.run=TestWindowsProcessTreeHelper")
	command.Env = append(os.Environ(),
		"PORTO_PROCESS_TREE_HELPER=parent",
		"PORTO_PROCESS_TREE_PID_FILE="+pidFile,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Kill(command) })
	childPID := waitForChildPID(t, pidFile)
	if err := Kill(command); err != nil {
		t.Fatalf("kill process tree: %v", err)
	}
	_ = command.Wait()
	waitForProcessExit(t, childPID)
}

func TestWindowsProcessTreeHelper(t *testing.T) {
	role := os.Getenv("PORTO_PROCESS_TREE_HELPER")
	if role == "" {
		return
	}
	if role == "parent" {
		executable, err := os.Executable()
		if err != nil {
			os.Exit(1)
		}
		child := exec.Command(executable, "-test.run=TestWindowsProcessTreeHelper")
		child.Env = append(os.Environ(), "PORTO_PROCESS_TREE_HELPER=child")
		if err := child.Start(); err != nil {
			os.Exit(1)
		}
		if err := os.WriteFile(os.Getenv("PORTO_PROCESS_TREE_PID_FILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(1)
		}
	}
	time.Sleep(10 * time.Minute)
}

func waitForChildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(string(data))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("child process did not start")
	return 0
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
		if err != nil {
			return
		}
		var exitCode uint32
		err = windows.GetExitCodeProcess(handle, &exitCode)
		_ = windows.CloseHandle(handle)
		if err != nil || exitCode != stillActive {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("child process %d is still running", pid)
}
