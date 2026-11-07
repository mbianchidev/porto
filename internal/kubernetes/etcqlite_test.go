package kubernetes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/etcqlite"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/vm"
)

type datastoreFixture struct {
	connection etcqlite.Connection
	backup     etcqlite.Backup
	failure    error
	calls      int
}

func (f *datastoreFixture) Connection(context.Context, string, string) (etcqlite.Connection, error) {
	f.calls++
	return f.connection, f.failure
}

func (f *datastoreFixture) Backup(context.Context, string, string) (etcqlite.Backup, error) {
	f.calls++
	return f.backup, f.failure
}

func syntheticConnection() etcqlite.Connection {
	return etcqlite.Connection{
		ClusterID: "18446744073709551001", Fingerprint: strings.Repeat("a", 64),
		Endpoints: []string{"https://host.lima.internal:52479"},
		CA:        []byte("synthetic CA"), Certificate: []byte("synthetic client certificate"), Key: []byte("synthetic private key"),
	}
}

func TestEtcqliteProviderDefaultsAndOtherDistributionsAreUntouched(t *testing.T) {
	fixture := &datastoreFixture{connection: syntheticConnection()}
	provisioner := NewClusterProvisioner(nil, newFakeRunner(), t.TempDir())
	provisioner.SetEtcqliteProvider(fixture)
	request := ClusterRequest{Name: "synthetic", Provider: "k3s"}
	if connection, err := provisioner.resolveEtcqlite(context.Background(), &request, true); err != nil || connection != nil || fixture.calls != 0 {
		t.Fatalf("default datastore did external work: %+v %v %d", connection, err, fixture.calls)
	}
	for _, provider := range []string{"k0s", "kind"} {
		request := ClusterRequest{Name: "synthetic", Provider: provider, Etcqlite: &EtcqliteDatastore{Name: "synthetic-db"}}
		if _, err := provisioner.Create(context.Background(), request); !errors.Is(err, etcqlite.ErrInvalid) {
			t.Fatalf("%s claimed external-etcd support: %v", provider, err)
		}
	}
	if fixture.calls != 0 {
		t.Fatal("unsupported provider contacted a datastore")
	}
}

func TestK3sEtcqliteBindingIsPinnedAndRestartCannotRetarget(t *testing.T) {
	runner := newFakeRunner()
	provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
	fixture := &datastoreFixture{connection: syntheticConnection()}
	provisioner.SetEtcqliteProvider(fixture)
	request := ClusterRequest{Name: "synthetic", Provider: "k3s", Etcqlite: &EtcqliteDatastore{Name: "synthetic-db", ClusterID: fixture.connection.ClusterID}}
	connection, err := provisioner.resolveEtcqlite(context.Background(), &request, true)
	if err != nil || connection == nil || request.Etcqlite.ClusterID != fixture.connection.ClusterID || request.Etcqlite.Fingerprint != fixture.connection.Fingerprint {
		t.Fatalf("datastore binding was not pinned: %+v %v", request, err)
	}
	if err := provisioner.writeClusterMetadata(request); err != nil {
		t.Fatal(err)
	}
	if err := provisioner.ProtectEtcqliteLifecycle(context.Background(), request.Etcqlite.Name, request.Etcqlite.ClusterID); !errors.Is(err, etcqlite.ErrConflict) {
		t.Fatalf("bound datastore was not protected: %v", err)
	}
	fixture.connection.Fingerprint = strings.Repeat("b", 64)
	if _, err := provisioner.Start(context.Background(), request.Name); !errors.Is(err, etcqlite.ErrConflict) {
		t.Fatalf("restart retargeted a changed endpoint/CA: %v", err)
	}
	if len(runner.commands) != 0 {
		t.Fatal("identity mismatch started or modified an existing VM")
	}
}

func TestK3sEtcqliteCredentialsAndReadinessPrecedeInstaller(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "unreachable"}[unavailable], func(t *testing.T) {
			runner := newFakeRunner()
			if unavailable {
				runner.handler = func(command runtimes.Command) ([]byte, error) {
					if slices.Contains(command.Args, "curl") && slices.Contains(command.Args, "https://host.lima.internal:52479/readyz") {
						return nil, errors.New("synthetic guest TLS/reachability failure")
					}
					return nil, nil
				}
			}
			provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
			connection := syntheticConnection()
			err := provisioner.installK3s(context.Background(), "porto-synthetic-server-1", "", true, "", "", nil, nil, &connection)
			if unavailable {
				if err == nil || !strings.Contains(err.Error(), "no installer was run") {
					t.Fatalf("guest failure hidden: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			writes := 0
			for _, command := range runner.commands {
				if len(command.Stdin) > 0 {
					writes++
					if !strings.Contains(strings.Join(command.Args, " "), "umask 077; cat > /etc/rancher/k3s/porto-etcqlite/") {
						t.Fatal("credentials did not use private stdin-based files")
					}
					if strings.Contains(strings.Join(command.Args, " "), "synthetic private key") {
						t.Fatal("private credentials leaked into arguments")
					}
				}
				if slices.Contains(command.Args, "/tmp/porto-install-k3s.sh") {
					if unavailable {
						t.Fatal("installer ran before datastore readiness")
					}
					for _, flag := range []string{"--datastore-endpoint", "--datastore-cafile", "--datastore-certfile", "--datastore-keyfile"} {
						if !slices.Contains(command.Args, flag) {
							t.Fatalf("external-etcd contract omitted %s", flag)
						}
					}
				}
			}
			if writes != 3 {
				t.Fatalf("expected exactly private CA/cert/key staging, got %d writes", writes)
			}
		})
	}
}

func TestNativeBackupFailurePreventsK3sDeletionAndPreservesRecoveryToken(t *testing.T) {
	runner := newFakeRunner()
	runner.instances["porto-synthetic-server-1"] = true
	provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
	connection := syntheticConnection()
	fixture := &datastoreFixture{connection: connection, failure: errors.New("synthetic native integrity failure")}
	provisioner.SetEtcqliteProvider(fixture)
	request := ClusterRequest{Name: "synthetic", Provider: "k3s", Etcqlite: &EtcqliteDatastore{
		Name: "synthetic-db", ClusterID: connection.ClusterID, Fingerprint: connection.Fingerprint,
	}}
	if err := provisioner.writeClusterMetadata(request); err != nil {
		t.Fatal(err)
	}
	if err := provisioner.Delete(context.Background(), request.Name); err == nil {
		t.Fatal("k3s VM deletion ignored native recovery failure")
	}
	if !runner.instances["porto-synthetic-server-1"] {
		t.Fatal("existing VM was deleted after failed native backup")
	}
	fixture.failure = nil
	fixture.backup = etcqlite.Backup{Path: t.TempDir(), ClusterID: connection.ClusterID, Snapshots: []etcqlite.Snapshot{{Member: "synthetic"}}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if strings.HasSuffix(strings.Join(command.Args, " "), "cat /var/lib/rancher/k3s/server/token") {
			return []byte("synthetic-k3s-recovery-token\n"), nil
		}
		return nil, nil
	}
	path, err := provisioner.backupEtcqliteDatastore(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(path, "k3s-server-token"))
	if err != nil || string(token) != "synthetic-k3s-recovery-token\n" {
		t.Fatal("Kubernetes recovery token was not preserved privately")
	}
}
