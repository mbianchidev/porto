package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type progressEngineRunner struct {
	base   *engineInstallRunner
	stream func(context.Context, runtimes.Command, func(runtimes.OutputChunk) error) ([]byte, error)
	run    func(context.Context, runtimes.Command) ([]byte, error)
}

func (r *progressEngineRunner) Run(ctx context.Context, command runtimes.Command) ([]byte, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if r.run != nil {
		return r.run(ctx, command)
	}
	return r.base.Run(ctx, command)
}

func (r *progressEngineRunner) RunStreaming(ctx context.Context, command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
	if r.stream != nil {
		return r.stream(ctx, command, emit)
	}
	return r.Run(ctx, command)
}

func progressEngineFixture(t *testing.T, existing bool) (*Manager, *progressEngineRunner) {
	t.Helper()
	t.Setenv("PORTO_HOME", t.TempDir())
	runner := &progressEngineRunner{base: &engineInstallRunner{created: existing, ownerID: "test-owner"}}
	manager := NewWithStateDir(runner, t.TempDir())
	manager.dialBuildKit = workingBuildKitDialer(t)
	manager.lookPath = func(name string) (string, error) {
		if name == "limactl" {
			return name, nil
		}
		return "", exec.ErrNotFound
	}
	if existing {
		if err := manager.writeEngineState(engineState{
			Mode: "lima", Instance: engineInstanceName, OwnerID: "test-owner",
		}); err != nil {
			t.Fatal(err)
		}
	}
	return manager, runner
}

func TestEngineInstallProgressPrecedesCompletionAndOmitsRawOutput(t *testing.T) {
	manager, runner := progressEngineFixture(t, false)
	release := make(chan struct{})
	defer close(release)
	runner.stream = func(ctx context.Context, command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
		output, err := runner.Run(ctx, command)
		if err != nil {
			return output, err
		}
		if command.Args[0] == "start" {
			for _, data := range []string{"Downlo", "ading https://user:super-secret@registry.invalid/guest?token=hidden\n"} {
				if err := emit(runtimes.OutputChunk{Stream: "stderr", Data: []byte(data)}); err != nil {
					return nil, err
				}
			}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, context.Cause(ctx)
			}
		} else if err := emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte("Get:1 https://mirror.invalid qemu-user-static\n")}); err != nil {
			return nil, err
		}
		return output, nil
	}
	events := make(chan EngineInstallProgress, 32)
	done := make(chan error, 1)
	go func() {
		status, err := manager.InstallEngineWithProgress(context.Background(), func(progress EngineInstallProgress) error {
			events <- progress
			return nil
		})
		if err == nil && !status.Available {
			err = errors.New("unverified engine was returned")
		}
		done <- err
	}()
	var observed []EngineInstallProgress
	for {
		select {
		case progress := <-events:
			observed = append(observed, progress)
			if progress.Stage == "downloading" {
				if progress.Component != "guest-image" {
					t.Fatalf("download component = %q", progress.Component)
				}
				select {
				case err := <-done:
					t.Fatalf("installation completed before provisioning finished: %v", err)
				default:
				}
				goto downloading
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no progress while the guest download was pending")
		}
	}
downloading:
	release <- struct{}{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("installation did not complete")
	}
	close(events)
	for progress := range events {
		observed = append(observed, progress)
	}
	encoded, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"super-secret", "registry.invalid", "token=", "hidden"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("raw command output leaked into progress: %s", encoded)
		}
	}
	for _, component := range []string{"multi-platform", "runtime-helper", "network", "containerd", "buildkit", "state"} {
		found := false
		for _, progress := range observed {
			found = found || progress.Component == component
		}
		if !found {
			t.Fatalf("missing %s progress: %s", component, encoded)
		}
	}
}

func TestEngineInstallProgressFailuresAreActionableAndSafe(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code string
	}{
		{"dns", &net.DNSError{Err: "no such host", Name: "secret.invalid"}, "network"},
		{"proxy", errors.New("proxyconnect tcp: user:secret@proxy.invalid: 407 Proxy Authentication Required"), "network"},
		{"download", errors.New("failed to download https://user:secret@mirror.invalid"), "network"},
		{"tls", errors.New("x509: certificate signed by unknown authority"), "network"},
		{"checksum", errors.New("download checksum mismatch for secret.invalid"), "network"},
		{"missing executable", &exec.Error{Name: "limactl", Err: exec.ErrNotFound}, "local-tool"},
		{"broken executable", &os.PathError{Op: "fork/exec", Path: "/private/secret/limactl", Err: os.ErrPermission}, "local-tool"},
		{"wrong architecture", errors.New("bad CPU type in executable"), "local-tool"},
		{"backend", errors.Join(ErrUnavailable, errors.New("connection refused to private backend")), "backend"},
		{"ownership", errors.New("engine is not owned by Porto"), "ownership"},
		{"cancelled", context.Canceled, "cancelled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := classifyEngineInstallFailure(test.err, EngineInstallProgress{Stage: "verifying", Component: "lima"})
			if failure.Code != test.code || !strings.Contains(strings.ToLower(failure.Message), "retry") {
				t.Fatalf("failure = %+v, want actionable %q", failure, test.code)
			}
			if strings.Contains(failure.Message, "secret") || strings.Contains(failure.Message, "private") {
				t.Fatalf("unsafe failure message: %s", failure.Message)
			}
		})
	}
}

func TestEngineInstallProgressNetworkFailureCleansOnlyNewEngine(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "existing"}[existing], func(t *testing.T) {
			manager, runner := progressEngineFixture(t, existing)
			runner.stream = func(ctx context.Context, command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
				if _, err := runner.Run(ctx, command); err != nil {
					return nil, err
				}
				if err := emit(runtimes.OutputChunk{Stream: "stderr", Data: []byte("Downloading https://user:secret@mirror.invalid\n")}); err != nil {
					return nil, err
				}
				return []byte("proxyconnect tcp: 407 Proxy Authentication Required https://user:secret@proxy.invalid"), errors.New("exit status 1")
			}
			status, err := manager.InstallEngineWithProgress(context.Background(), func(EngineInstallProgress) error { return nil })
			if err == nil || status.Available {
				t.Fatalf("failed download marked ready: %+v, %v", status, err)
			}
			failure := EngineInstallationFailure(err)
			if failure.Code != "network" || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe or unclassified network error: %+v, %v", failure, err)
			}
			assertProgressEnginePreserved(t, manager, runner, existing)
		})
	}
}

func TestEngineInstallProgressObserverFailuresReleaseResources(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, component := range []string{"ownership", "multi-platform", "runtime-helper", "network", "containerd", "buildkit", "state"} {
			t.Run(map[bool]string{false: "new/", true: "existing/"}[existing]+component, func(t *testing.T) {
				manager, runner := progressEngineFixture(t, existing)
				observerError := errors.New("progress connection lost")
				_, err := manager.InstallEngineWithProgress(context.Background(), func(progress EngineInstallProgress) error {
					if progress.Component == component {
						return observerError
					}
					return nil
				})
				if !errors.Is(err, observerError) {
					t.Fatalf("observer failure was ignored: %v", err)
				}
				assertProgressEnginePreserved(t, manager, runner, existing)
				lockContext, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				lockContext = context.WithValue(lockContext, engineInstallProgressKey{}, &engineInstallObserver{})
				lock, lockErr := acquireEngineInstallLockContext(
					lockContext, filepath.Join(manager.stateDir, engineLockFile))
				if lockErr != nil {
					t.Fatal(lockErr)
				}
				if err := lock.Close(); err != nil {
					t.Fatal(err)
				}
				if !manager.installMu.TryLock() {
					t.Fatal("observer failure retained the installation mutex")
				}
				manager.installMu.Unlock()
			})
		}
	}
}

func TestEngineInstallProgressCancellationStopsStreamingAndCleansUp(t *testing.T) {
	manager, runner := progressEngineFixture(t, false)
	started := make(chan struct{})
	runner.stream = func(ctx context.Context, command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
		if _, err := runner.Run(ctx, command); err != nil {
			return nil, err
		}
		if err := emit(runtimes.OutputChunk{Stream: "stderr", Data: []byte("Downloading guest image\n")}); err != nil {
			return nil, err
		}
		close(started)
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := manager.InstallEngineWithProgress(ctx, func(EngineInstallProgress) error { return nil })
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("streaming did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not release provisioning")
	}
	assertProgressEnginePreserved(t, manager, runner, false)
}

func TestEngineInstallProgressCanCancelWhileWaitingForLocks(t *testing.T) {
	for _, fileLock := range []bool{false, true} {
		t.Run(map[bool]string{false: "mutex", true: "file lock"}[fileLock], func(t *testing.T) {
			manager, _ := progressEngineFixture(t, false)
			if fileLock {
				lock, err := acquireEngineInstallLock(filepath.Join(manager.stateDir, engineLockFile))
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			} else {
				manager.installMu.Lock()
				defer manager.installMu.Unlock()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := manager.InstallEngineWithProgress(ctx, func(EngineInstallProgress) error { return nil })
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("lock cancellation = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled installation remained blocked on a lock")
			}
		})
	}
}

func TestEngineInstallProgressExistingEngineDoesNotDownloadOrRestart(t *testing.T) {
	manager, runner := progressEngineFixture(t, true)
	runner.stream = func(ctx context.Context, command runtimes.Command, _ func(runtimes.OutputChunk) error) ([]byte, error) {
		if command.Args[0] == "start" {
			t.Error("verified running engine was restarted")
		}
		return runner.Run(ctx, command)
	}
	status, err := manager.InstallEngineWithProgress(context.Background(), func(progress EngineInstallProgress) error {
		if progress.Stage == "downloading" {
			t.Errorf("existing verified guest unnecessarily downloaded %s", progress.Component)
		}
		return nil
	})
	if err != nil || !status.Available || runner.base.removed {
		t.Fatalf("offline existing engine = %+v, %v", status, err)
	}
	for _, command := range runner.base.commands {
		if command.Args[0] == "start" || command.Args[0] == "delete" {
			t.Fatalf("existing engine was recreated: %+v", command)
		}
	}
}

func TestEngineInstallProgressExistingDirectEngineUsesOnlyVerification(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{"nerdctl version": []byte("nerdctl version fixture")}}
	manager := NewWithStateDir(runner, t.TempDir())
	manager.lookPath = func(name string) (string, error) {
		if name == "nerdctl" {
			return name, nil
		}
		return "", exec.ErrNotFound
	}
	manager.dialBuildKit = workingBuildKitDialer(t)
	status, err := manager.InstallEngineWithProgress(context.Background(), func(progress EngineInstallProgress) error {
		if progress.Stage == "downloading" || progress.Component == "lima" {
			t.Errorf("verified direct engine provisioned guest components: %+v", progress)
		}
		return nil
	})
	if err != nil || !status.Available {
		t.Fatalf("direct installation = %+v, %v", status, err)
	}
	if len(runner.commands) != 1 || strings.Join(runner.commands[0].Args, " ") != "version" {
		t.Fatalf("unexpected direct commands: %+v", runner.commands)
	}
}

func TestEngineInstallProgressDoesNotFallBackFromUnsupportedStreaming(t *testing.T) {
	base := &engineInstallRunner{}
	manager := NewWithStateDir(base, t.TempDir())
	manager.lookPath = func(name string) (string, error) {
		if name == "limactl" {
			return name, nil
		}
		return "", exec.ErrNotFound
	}
	_, err := manager.InstallEngineWithProgress(context.Background(), func(EngineInstallProgress) error { return nil })
	if !errors.Is(err, ErrUnsupported) || base.created || base.removed {
		t.Fatalf("unsupported streaming mutated a guest: %v, created=%t, removed=%t", err, base.created, base.removed)
	}
}

func TestEngineInstallProgressLocalToolFailuresAreNotBackendFailures(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "broken", true: "missing"}[missing], func(t *testing.T) {
			manager, runner := progressEngineFixture(t, false)
			if missing {
				manager.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
			} else {
				runner.run = func(context.Context, runtimes.Command) ([]byte, error) {
					return nil, &os.PathError{Op: "fork/exec", Path: "/synthetic/limactl", Err: os.ErrPermission}
				}
			}
			status, err := manager.InstallEngineWithProgress(context.Background(), func(EngineInstallProgress) error { return nil })
			if err == nil || status.Available || EngineInstallationFailure(err).Code != "local-tool" {
				t.Fatalf("local executable failure = %+v, %v", status, err)
			}
			if runner.base.created || runner.base.removed {
				t.Fatal("local tool failure modified an engine")
			}
		})
	}
}

func TestEngineInstallProgressBackendMustPassFinalVerification(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "existing"}[existing], func(t *testing.T) {
			manager, runner := progressEngineFixture(t, existing)
			working := workingBuildKitDialer(t)
			calls := 0
			manager.dialBuildKit = func(ctx context.Context) (net.Conn, error) {
				calls++
				if calls > 1 {
					return nil, ErrUnavailable
				}
				return working(ctx)
			}
			status, err := manager.InstallEngineWithProgress(context.Background(), func(EngineInstallProgress) error { return nil })
			if err == nil || status.Available || EngineInstallationFailure(err).Code != "backend" {
				t.Fatalf("unavailable backend marked complete: %+v, %v", status, err)
			}
			assertProgressEnginePreserved(t, manager, runner, existing)
		})
	}
}

func TestEngineInstallStartupFailureStopsLegacyAndStreamingProvisioning(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "stream"}[streamed], func(t *testing.T) {
			manager, runner := progressEngineFixture(t, false)
			startError := errors.New("synthetic start failed")
			runner.run = func(ctx context.Context, command runtimes.Command) ([]byte, error) {
				if command.Args[0] == "start" {
					_, _ = runner.base.Run(ctx, command)
					return nil, startError
				}
				return runner.base.Run(ctx, command)
			}
			var err error
			if streamed {
				_, err = manager.InstallEngineWithProgress(context.Background(), func(EngineInstallProgress) error { return nil })
			} else {
				_, err = manager.InstallEngine(context.Background())
			}
			if !errors.Is(err, startError) {
				t.Fatalf("startup failure was shadowed or hidden: %v", err)
			}
			if runner.base.ownerID != "test-owner" || runner.base.binfmtConfigured {
				t.Fatal("failed startup continued with ownership or guest provisioning")
			}
			assertProgressEnginePreserved(t, manager, runner, false)
		})
	}
}

func assertProgressEnginePreserved(t *testing.T, manager *Manager, runner *progressEngineRunner, existing bool) {
	t.Helper()
	if runner.base.removed == existing {
		t.Fatalf("removed=%t, existing=%t", runner.base.removed, existing)
	}
	state, err := manager.readEngineState()
	if existing {
		if err != nil || state.OwnerID != "test-owner" || state.Mode != "lima" {
			t.Fatalf("existing engine state changed: %+v, %v", state, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed new setup persisted engine state: %+v, %v", state, err)
	}
}
