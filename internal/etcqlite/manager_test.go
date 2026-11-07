package etcqlite

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/filelock"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type fixtureRunner struct {
	commands []runtimes.Command
	statuses map[string]SnapshotStatus
	fail     bool
}

func argument(args []string, name string) string {
	for index, value := range args {
		if value == name && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

func (r *fixtureRunner) Run(_ context.Context, command runtimes.Command) ([]byte, error) {
	r.commands = append(r.commands, command)
	args := command.Args
	switch args[0] {
	case "init":
		directory := argument(args, "--data-dir")
		clusterID, memberID := argument(args, "--cluster-id"), argument(args, "--member-id")
		if configPath := argument(args, "--cluster-config"); configPath != "" {
			data, err := os.ReadFile(configPath)
			if err != nil {
				return nil, err
			}
			var config genesis
			if err := json.Unmarshal(data, &config); err != nil {
				return nil, err
			}
			clusterID = strconv.FormatUint(config.ClusterID, 10)
		}
		if err := os.Mkdir(directory, 0o700); err != nil {
			return nil, err
		}
		if err := writeExclusive(filepath.Join(directory, "node.lock"), nil); err != nil {
			return nil, err
		}
		return nil, writeJSONAtomic(filepath.Join(directory, "synthetic-identity.json"), map[string]string{"cluster": clusterID, "member": memberID})
	case "snapshot":
		if args[1] == "status" {
			if r.fail {
				return []byte("synthetic integrity failure"), errors.New("invalid recovery image")
			}
			return json.Marshal(r.statuses[argument(args, "--input")])
		}
		data, err := os.ReadFile(filepath.Join(argument(args, "--data-dir"), "synthetic-identity.json"))
		if err != nil {
			return nil, err
		}
		var identity map[string]string
		if err := json.Unmarshal(data, &identity); err != nil {
			return nil, err
		}
		image, status := syntheticSnapshot()
		status.ClusterID, err = strconv.ParseUint(identity["cluster"], 10, 64)
		if err != nil {
			return nil, err
		}
		status.MemberID, err = strconv.ParseUint(identity["member"], 10, 64)
		if err != nil {
			return nil, err
		}
		path := argument(args, "--output")
		r.statuses[path] = status
		return nil, writeExclusive(path, image)
	}
	return nil, errors.New("unexpected synthetic native command")
}

func fixtureManager(t *testing.T) (*Manager, *fixtureRunner) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("native lifecycle profile is macOS/Linux")
	}
	t.Setenv("PORTO_ETCQLITE_BINARY", "")
	t.Setenv("PORTO_ETCQLITE_SNAPSHOT_BINARY", "")
	t.Setenv("PORTO_ETCQLITE_TOKEN", "")
	runner := &fixtureRunner{statuses: make(map[string]SnapshotStatus)}
	manager := New(filepath.Join(t.TempDir(), "etcqlite"), runner)
	manager.SetEnabled(true)
	bin := t.TempDir()
	for _, name := range []string{"etcqlite", "etcqlite-snapshot"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("synthetic executable; mocked runner only"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	manager.lookPath = func(name string) (string, error) { return filepath.Join(bin, name), nil }
	return manager, runner
}

func TestDisabledIntegrationDoesNoExternalOrFilesystemWork(t *testing.T) {
	runner := &fixtureRunner{}
	root := filepath.Join(t.TempDir(), "must-not-exist")
	manager := New(root, runner)
	manager.lookPath = func(string) (string, error) { t.Fatal("disabled binary lookup"); return "", nil }
	manager.identify = func(int) (*ProcessIdentity, error) { t.Fatal("disabled process inspection"); return nil, nil }
	ctx := context.Background()
	if status, err := manager.Status(ctx); err != nil || status.State != "disabled" || len(status.Instances) != 0 {
		t.Fatalf("disabled status: %+v %v", status, err)
	}
	calls := []func() error{
		func() error { _, err := manager.Create(ctx, CreateRequest{Name: "synthetic"}); return err },
		func() error { _, err := manager.Start(ctx, "synthetic", "123"); return err },
		func() error { _, err := manager.Stop(ctx, "synthetic", "123"); return err },
		func() error { _, err := manager.Backup(ctx, "synthetic", "123"); return err },
		func() error { _, err := manager.Delete(ctx, "synthetic", "123", true); return err },
		func() error { _, err := manager.Restore(ctx, "synthetic", "123"); return err },
		func() error { _, err := manager.Connection(ctx, "synthetic", "123"); return err },
		func() error { _, err := manager.Logs(ctx, "synthetic", "stdout", 10); return err },
		func() error { _, err := manager.Backups(ctx); return err },
	}
	for _, call := range calls {
		if err := call(); !errors.Is(err, ErrDisabled) {
			t.Fatalf("disabled operation error: %v", err)
		}
	}
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) || len(runner.commands) != 0 {
		t.Fatalf("disabled integration touched state or ran commands: %v %+v", err, runner.commands)
	}
}

func TestCreationPinsPrivatePerMemberIdentityAndDoesNotStart(t *testing.T) {
	for _, count := range []int{1, 3} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			manager, runner := fixtureManager(t)
			instance, err := manager.Create(context.Background(), CreateRequest{Name: "synthetic", Members: count})
			if err != nil {
				t.Fatal(err)
			}
			if instance.State != "stopped" || instance.DesiredRunning || len(instance.Members) != count || len(runner.commands) != count {
				t.Fatalf("creation did unexpected lifecycle work: %+v %+v", instance, runner.commands)
			}
			loaded, err := manager.loadInstance(instance.Name)
			if err != nil || !reflect.DeepEqual(instance, loaded) {
				t.Fatalf("persistent identity changed: %+v %v", loaded, err)
			}
			for _, member := range instance.Members {
				if member.Process != nil || !member.Initialized {
					t.Fatalf("unexpected member lifecycle: %+v", member)
				}
				pair, err := tls.LoadX509KeyPair(
					filepath.Join(manager.instancePath(instance.Name), "tls", member.Name+".pem"),
					filepath.Join(manager.instancePath(instance.Name), "tls", member.Name+"-key.pem"),
				)
				if err != nil {
					t.Fatal(err)
				}
				leaf, err := x509.ParseCertificate(pair.Certificate[0])
				if err != nil {
					t.Fatal(err)
				}
				etag, err := identityETag(instance.ClusterID, member.ID)
				if err != nil {
					t.Fatal(err)
				}
				want := "spiffe://etcqlite/" + etag[1:17] + "/" + etag[18:34]
				if len(leaf.URIs) != 1 || leaf.URIs[0].String() != want {
					t.Fatalf("peer certificate URI = %v, want %s", leaf.URIs, want)
				}
				info, err := os.Stat(filepath.Join(manager.instancePath(instance.Name), "tls", member.Name+"-key.pem"))
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("private key mode: %v %v", info, err)
				}
			}
			if _, err := manager.Create(context.Background(), CreateRequest{Name: instance.Name}); !errors.Is(err, ErrConflict) {
				t.Fatalf("existing cluster state was reused: %v", err)
			}
			if len(runner.commands) != count {
				t.Fatal("duplicate create reran initialization")
			}
		})
	}
}

func TestInterruptedInitializationIsNeverReinitializedOnRestart(t *testing.T) {
	manager, runner := fixtureManager(t)
	instance, err := manager.Create(context.Background(), CreateRequest{Name: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	instance.Members[0].Initialized = false
	instance.DesiredRunning = true
	if err := manager.writeInstance(instance); err != nil {
		t.Fatal(err)
	}
	before := len(runner.commands)
	if err := manager.Reconcile(context.Background()); err == nil || !strings.Contains(err.Error(), "will not be reinitialized") {
		t.Fatalf("interrupted initialization error: %v", err)
	}
	if len(runner.commands) != before {
		t.Fatal("restart reran native initialization")
	}
}

func TestReusedPIDIsNeverSignaled(t *testing.T) {
	manager, _ := fixtureManager(t)
	manager.identify = func(pid int) (*ProcessIdentity, error) {
		return &ProcessIdentity{PID: pid, Birth: "different-process-birth", Executable: "/synthetic/etcqlite"}, nil
	}
	manager.terminate = func(int) error { t.Fatal("reused PID was terminated"); return nil }
	manager.kill = func(int) error { t.Fatal("reused PID was killed"); return nil }
	if err := manager.stopIdentity(context.Background(), &ProcessIdentity{PID: 1234, Birth: "original", Executable: "/synthetic/etcqlite"}); err != nil {
		t.Fatal(err)
	}
}

func TestDeletionRequiresNativeVerificationAndRetainsPriorBackups(t *testing.T) {
	manager, runner := fixtureManager(t)
	instance, err := manager.Create(context.Background(), CreateRequest{Name: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.Backup(context.Background(), instance.Name, instance.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(first.Snapshots[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	runner.fail = true
	if _, err := manager.Delete(context.Background(), instance.Name, instance.ClusterID, true); err == nil {
		t.Fatal("destructive lifecycle accepted an invalid native backup")
	}
	if _, err := os.Stat(manager.instancePath(instance.Name)); err != nil {
		t.Fatal("instance data was removed after failed verification")
	}
	after, err := os.ReadFile(first.Snapshots[0].Path)
	if err != nil || string(after) != string(before) {
		t.Fatal("previous verified recovery image was overwritten")
	}
	backups, err := manager.Backups(context.Background())
	if err != nil || len(backups) != 1 || backups[0].ID != first.ID {
		t.Fatalf("unverified artifact was published as a backup: %+v %v", backups, err)
	}
}

func TestReversibleRetentionHoldsNativeWriterLockAndPreservesIdentity(t *testing.T) {
	manager, _ := fixtureManager(t)
	instance, err := manager.Create(context.Background(), CreateRequest{Name: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(manager.memberPath(instance, instance.Members[0]), "data", "node.lock")
	before, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := filelock.TryAcquireExisting(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Delete(context.Background(), instance.Name, instance.ClusterID, true); err == nil {
		t.Fatal("retention ignored an active native writer")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	retained, err := manager.Delete(context.Background(), instance.Name, instance.ClusterID, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manager.instancePath(instance.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("instance was not retained separately: %v", err)
	}
	after, err := os.Stat(filepath.Join(retained, "members", instance.Members[0].Name, "data", "node.lock"))
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("retention replaced or unlinked the writer-lock inode")
	}
	restored, err := manager.Restore(context.Background(), instance.Name, instance.ClusterID)
	if err != nil || restored.ClusterID != instance.ClusterID || restored.DesiredRunning {
		t.Fatalf("restore changed identity or started a runtime: %+v %v", restored, err)
	}
	if _, err := manager.Restore(context.Background(), instance.Name, instance.ClusterID); err == nil {
		t.Fatal("restoration overwrote an existing instance")
	}
}

func TestDatastoreBindingAndIdentityGuardLifecycle(t *testing.T) {
	manager, runner := fixtureManager(t)
	instance, err := manager.Create(context.Background(), CreateRequest{Name: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Connection(context.Background(), instance.Name, instance.ClusterID); err == nil || !strings.Contains(err.Error(), "guest-reachable") {
		t.Fatalf("loopback datastore was silently retargeted: %v", err)
	}
	commands := len(runner.commands)
	manager.SetGuard(func(context.Context, string, string) error {
		return errors.New("synthetic k3s cluster still uses this datastore")
	})
	if _, err := manager.Stop(context.Background(), instance.Name, instance.ClusterID); err == nil {
		t.Fatal("bound datastore was stopped")
	}
	if _, err := manager.Delete(context.Background(), instance.Name, instance.ClusterID, true); err == nil {
		t.Fatal("bound datastore was deleted")
	}
	if len(runner.commands) != commands {
		t.Fatal("binding rejection performed external work")
	}
	persisted := false
	if err := manager.Configure(context.Background(), false, func() error { persisted = true; return nil }); err == nil || persisted {
		t.Fatal("bound integration was disabled")
	}
	if _, err := manager.Start(context.Background(), instance.Name, "foreign"); !errors.Is(err, ErrConflict) {
		t.Fatalf("identity mismatch not rejected: %v", err)
	}
}

func TestInstalledNativeLifecycleWithSyntheticData(t *testing.T) {
	binary := os.Getenv("PORTO_ETCQLITE_TEST_BINARY")
	if binary == "" || (runtime.GOOS != "darwin" && runtime.GOOS != "linux") {
		t.Skip("set PORTO_ETCQLITE_TEST_BINARY to opt into isolated native executable acceptance")
	}
	t.Setenv("PORTO_ETCQLITE_TOKEN", "")
	t.Setenv("PORTO_ETCQLITE_BINARY", binary)
	t.Setenv("PORTO_ETCQLITE_SNAPSHOT_BINARY", filepath.Join(filepath.Dir(binary), "etcqlite-snapshot"))
	for _, count := range []int{1, 3} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "native")
			manager := New(root, nil)
			manager.SetEnabled(true)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			instance, err := manager.Create(ctx, CreateRequest{Name: "synthetic-native", Members: count})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if _, err := manager.Stop(cleanup, instance.Name, instance.ClusterID); err != nil {
					t.Errorf("stop only this test's owned native members: %v", err)
				}
			})
			instance, err = manager.Start(ctx, instance.Name, instance.ClusterID)
			if err != nil || instance.State != "ready" {
				t.Fatalf("native readiness: %+v %v", instance, err)
			}
			restarted := New(root, nil)
			restarted.SetEnabled(true)
			if err := restarted.Reconcile(ctx); err != nil {
				t.Fatalf("restart ownership reconciliation: %v", err)
			}
			persisted, err := restarted.loadInstance(instance.Name)
			if err != nil || !reflect.DeepEqual(persisted.Members, instance.Members) {
				t.Fatalf("restart replaced member identity or processes: %+v %v", persisted, err)
			}
			backup, err := restarted.Backup(ctx, instance.Name, instance.ClusterID)
			if err != nil || len(backup.Snapshots) != count {
				t.Fatalf("verified native backup: %+v %v", backup, err)
			}
			for _, snapshot := range backup.Snapshots {
				image, err := os.ReadFile(snapshot.Path)
				if err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256(image[:len(image)-32])
				if hex.EncodeToString(sum[:]) != snapshot.Status.SHA256 {
					t.Fatal("native payload digest did not cover the downloaded recovery image")
				}
			}
			if _, err := restarted.Stop(ctx, instance.Name, instance.ClusterID); err != nil {
				t.Fatal(err)
			}
			retained, err := restarted.Delete(ctx, instance.Name, instance.ClusterID, true)
			if err != nil || retained == "" {
				t.Fatalf("native-verified reversible deletion: %s %v", retained, err)
			}
			restored, err := restarted.Restore(ctx, instance.Name, instance.ClusterID)
			if err != nil || restored.ClusterID != instance.ClusterID || restored.DesiredRunning {
				t.Fatalf("isolated retained restoration: %+v %v", restored, err)
			}
		})
	}
}
