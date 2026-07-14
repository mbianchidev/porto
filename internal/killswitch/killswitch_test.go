package killswitch

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/app"
)

type fakeRunner struct {
	mu    sync.Mutex
	paths map[string]string
	runs  []fakeRun
	run   func(name string, args []string) (CommandOutput, error)
}

type fakeRun struct {
	name string
	args []string
}

func (f *fakeRunner) LookPath(name string) (string, error) {
	path, ok := f.paths[name]
	if !ok {
		return "", errors.New("not found")
	}
	return path, nil
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (CommandOutput, error) {
	f.mu.Lock()
	f.runs = append(f.runs, fakeRun{name: name, args: append([]string(nil), args...)})
	run := f.run
	f.mu.Unlock()
	if run != nil {
		return run(name, args)
	}
	return CommandOutput{}, nil
}

func (f *fakeRunner) recordedRuns() []fakeRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRun(nil), f.runs...)
}

type fakeInstaller struct {
	calls int
	err   error
}

func (f *fakeInstaller) Install(context.Context) error {
	f.calls++
	return f.err
}

func TestManagedPortsIncludesOnlyActiveProcesses(t *testing.T) {
	projects := []app.Project{
		{Name: "running", Status: "running", PID: 101, Port: 41002},
		{Name: "duplicate", Status: "running", PID: 102, Port: 41002},
		{Name: "stopped", Status: "stopped", PID: 0, Port: 41003},
		{Name: "stale", Status: "running", PID: 0, Port: 41004},
		{Name: "daemon", Status: "running", PID: 103, Port: 37623},
		{Name: "router", Status: "running", PID: 104, Port: 37680},
		{Name: "second", Status: "running", PID: 105, Port: 41001},
	}

	got := ManagedPorts(projects)
	want := []int{41001, 41002}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ports = %v, want %v", got, want)
	}
}

func TestSyncUsesStdoutJSONAndKeepsStderrSeparate(t *testing.T) {
	runner := &fakeRunner{
		paths: map[string]string{"killswitchctl": "/Users/test/bin/killswitchctl"},
		run: func(_ string, _ []string) (CommandOutput, error) {
			return CommandOutput{
				Stdout: []byte(`{"version":"1.4.0","autoKillEnabled":true,"userPorts":[3000],"integrationPorts":{"porto":[41001,41002]},"effectivePorts":[3000,41001,41002]}`),
				Stderr: []byte("AppKit warning that must not corrupt JSON"),
			}, nil
		},
	}
	manager := newManager("darwin", runner, &fakeInstaller{}, func() (string, error) {
		return "/Users/test", nil
	})

	status, err := manager.Sync(context.Background(), []int{41002, 41001, 41002})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if status.State != "ready" || status.Version != "1.4.0" {
		t.Fatalf("status = %+v", status)
	}
	if status.AutoKillEnabled == nil || !*status.AutoKillEnabled {
		t.Fatalf("auto-kill status = %v", status.AutoKillEnabled)
	}
	if !reflect.DeepEqual(status.SyncedPorts, []int{41001, 41002}) {
		t.Fatalf("synced ports = %v", status.SyncedPorts)
	}
	wantRun := fakeRun{
		name: "/Users/test/bin/killswitchctl",
		args: []string{"dev-cleanup", "sync-ports", "--source", sourceName, "--ports", "41001,41002", "--json"},
	}
	if got := runner.recordedRuns(); !reflect.DeepEqual(got, []fakeRun{wantRun}) {
		t.Fatalf("runs = %+v, want %+v", got, []fakeRun{wantRun})
	}
}

func TestSyncPassesExplicitEmptyPortsToClearSource(t *testing.T) {
	runner := &fakeRunner{
		paths: map[string]string{"killswitchctl": "killswitchctl"},
		run: func(_ string, _ []string) (CommandOutput, error) {
			return CommandOutput{
				Stdout: []byte(`{"version":"1.4.0","autoKillEnabled":false,"userPorts":[],"integrationPorts":{},"effectivePorts":[]}`),
			}, nil
		},
	}
	manager := newManager("darwin", runner, &fakeInstaller{}, func() (string, error) {
		return "/Users/test", nil
	})

	if _, err := manager.Sync(context.Background(), nil); err != nil {
		t.Fatalf("clear sync: %v", err)
	}
	got := runner.recordedRuns()
	if len(got) != 1 {
		t.Fatalf("runs = %+v", got)
	}
	wantArgs := []string{"dev-cleanup", "sync-ports", "--source", sourceName, "--ports", "", "--json"}
	if !reflect.DeepEqual(got[0].args, wantArgs) {
		t.Fatalf("args = %q, want %q", got[0].args, wantArgs)
	}
}

func TestCleanupParsesSharedEngineResult(t *testing.T) {
	runner := &fakeRunner{
		paths: map[string]string{"killswitchctl": "killswitchctl"},
		run: func(_ string, _ []string) (CommandOutput, error) {
			return CommandOutput{
				Stdout: []byte(`{"version":"1.4.0","autoKillEnabled":true,"candidateCount":2,"killedCount":1,"killedProcesses":[{"pid":123,"command":"node vite","runtime":"node","ageHours":13.5}]}`),
			}, nil
		},
	}
	manager := newManager("darwin", runner, &fakeInstaller{}, func() (string, error) {
		return "/Users/test", nil
	})

	result, err := manager.Cleanup(context.Background())
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if !result.AutoKillEnabled || result.CandidateCount != 2 || result.KilledCount != 1 || len(result.KilledProcesses) != 1 || result.KilledProcesses[0].PID != 123 {
		t.Fatalf("result = %+v", result)
	}
}

func TestInstallVerifiesCLIStatus(t *testing.T) {
	installer := &fakeInstaller{}
	runner := &fakeRunner{
		paths: map[string]string{"killswitchctl": "/Users/test/bin/killswitchctl"},
		run: func(_ string, _ []string) (CommandOutput, error) {
			return CommandOutput{
				Stdout: []byte(`{"version":"1.4.0","autoKillEnabled":true,"userPorts":[],"integrationPorts":{},"effectivePorts":[]}`),
			}, nil
		},
	}
	manager := newManager("darwin", runner, installer, func() (string, error) {
		return "/Users/test", nil
	})

	status, err := manager.Install(context.Background())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if installer.calls != 1 || !status.Installed || status.Version != "1.4.0" {
		t.Fatalf("installer calls = %d, status = %+v", installer.calls, status)
	}
}

func TestRequestSyncCoalescesToLatestPorts(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	twoRuns := make(chan struct{})
	var runCount int
	var runMu sync.Mutex
	runner := &fakeRunner{
		paths: map[string]string{"killswitchctl": "killswitchctl"},
		run: func(_ string, args []string) (CommandOutput, error) {
			runMu.Lock()
			runCount++
			current := runCount
			runMu.Unlock()
			if current == 1 {
				close(firstStarted)
				<-releaseFirst
			}
			if current == 2 {
				close(twoRuns)
			}
			ports := args[5]
			return CommandOutput{
				Stdout: []byte(`{"version":"1.4.0","autoKillEnabled":true,"userPorts":[],"integrationPorts":{"porto":[` + ports + `]},"effectivePorts":[` + ports + `]}`),
			}, nil
		},
	}
	manager := newManager("darwin", runner, &fakeInstaller{}, func() (string, error) {
		return "/Users/test", nil
	})

	manager.RequestSync([]int{41001}, nil)
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first sync did not start")
	}
	manager.RequestSync([]int{41002}, nil)
	close(releaseFirst)
	select {
	case <-twoRuns:
	case <-time.After(time.Second):
		t.Fatal("latest sync did not run")
	}

	runs := runner.recordedRuns()
	if len(runs) != 2 || runs[1].args[5] != "41002" {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestUnsupportedPlatformDoesNotInvokeRunner(t *testing.T) {
	runner := &fakeRunner{paths: map[string]string{"killswitchctl": "killswitchctl"}}
	manager := newManager("linux", runner, &fakeInstaller{}, func() (string, error) {
		return "/home/test", nil
	})

	status, err := manager.Probe(context.Background())
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v", err)
	}
	if status.State != "unsupported" || len(runner.recordedRuns()) != 0 {
		t.Fatalf("status = %+v, runs = %+v", status, runner.recordedRuns())
	}
}

func TestDisabledStatusClearsStaleInstallationDetails(t *testing.T) {
	runner := &fakeRunner{
		paths: map[string]string{"killswitchctl": "killswitchctl"},
		run: func(_ string, _ []string) (CommandOutput, error) {
			return CommandOutput{
				Stdout: []byte(`{"version":"1.4.0","autoKillEnabled":true,"userPorts":[],"integrationPorts":{},"effectivePorts":[]}`),
			}, nil
		},
	}
	manager := newManager("darwin", runner, &fakeInstaller{}, func() (string, error) {
		return t.TempDir(), nil
	})
	if _, err := manager.Sync(context.Background(), nil); err != nil {
		t.Fatalf("sync: %v", err)
	}

	delete(runner.paths, "killswitchctl")
	status := manager.DisabledStatus()
	if status.Installed || status.BinaryPath != "" {
		t.Fatalf("disabled status kept stale installation details: %+v", status)
	}
}

func TestScriptInstallerUsesPinnedSourceAndRemovesTemporaryFile(t *testing.T) {
	var installerPath string
	runner := &fakeRunner{
		paths: map[string]string{
			"curl": "/usr/bin/curl",
			"bash": "/bin/bash",
			"env":  "/usr/bin/env",
		},
		run: func(name string, args []string) (CommandOutput, error) {
			switch name {
			case "/usr/bin/curl":
				for index, arg := range args {
					if arg == "--output" && index+1 < len(args) {
						installerPath = args[index+1]
						break
					}
				}
				if installerPath == "" {
					t.Fatal("curl output path was not provided")
				}
				if strings.Contains(args[len(args)-1], "/main/") || args[len(args)-1] != installScriptURL {
					t.Fatalf("installer URL = %q", args[len(args)-1])
				}
				if err := os.WriteFile(installerPath, []byte("#!/bin/bash\n"), 0o600); err != nil {
					t.Fatalf("write fake installer: %v", err)
				}
			case "/usr/bin/env":
				want := []string{"KILLSWITCH_INSTALL_MODE=release", "/bin/bash", installerPath}
				if !reflect.DeepEqual(args, want) {
					t.Fatalf("installer args = %q, want %q", args, want)
				}
				if _, err := os.Stat(installerPath); err != nil {
					t.Fatalf("installer file unavailable during execution: %v", err)
				}
			default:
				t.Fatalf("unexpected command %q", name)
			}
			return CommandOutput{}, nil
		},
	}

	if err := (&ScriptInstaller{runner: runner, url: installScriptURL}).Install(context.Background()); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(installerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary installer still exists: %v", err)
	}
}
