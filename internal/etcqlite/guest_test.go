package etcqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestInstalledNativeGuestReadiness(t *testing.T) {
	binary, machine := os.Getenv("PORTO_ETCQLITE_TEST_BINARY"), os.Getenv("PORTO_ETCQLITE_TEST_VM")
	if binary == "" || machine == "" || !Supported() {
		t.Skip("explicit native binary and an authorized isolated PORTO_ETCQLITE_TEST_VM are required")
	}
	if !namePattern.MatchString(machine) {
		t.Fatal("invalid explicitly selected qualification VM")
	}
	t.Setenv("PORTO_ETCQLITE_BINARY", binary)
	t.Setenv("PORTO_ETCQLITE_SNAPSHOT_BINARY", filepath.Join(filepath.Dir(binary), "etcqlite-snapshot"))
	t.Setenv("PORTO_ETCQLITE_TOKEN", "")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	manager := New(filepath.Join(t.TempDir(), "native-guest-probe"), nil)
	manager.SetEnabled(true)
	instance, err := manager.Create(ctx, CreateRequest{
		Name: "synthetic-guest", AdvertiseHost: "host.lima.internal", ListenHost: "0.0.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := manager.Stop(cleanup, instance.Name, instance.ClusterID); err != nil {
			t.Errorf("stop only the owned synthetic host member: %v", err)
		}
	})
	if _, err := manager.Start(ctx, instance.Name, instance.ClusterID); err != nil {
		t.Fatal(err)
	}
	id, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	directory := "/tmp/porto-etcqlite-probe-" + id
	run := func(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
		return (runtimes.ExecRunner{}).Run(ctx, runtimes.Command{
			Name: "limactl", Args: append([]string{"shell", machine, "--"}, args...), Stdin: stdin, StdoutOnly: true,
		})
	}
	if output, err := run(ctx, nil, "mkdir", "-m", "700", directory); err != nil {
		t.Fatal(runtimes.CommandError("create only the private probe directory in the selected VM", output, err))
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if output, err := run(cleanup, nil, "rm", "-r", "--", directory); err != nil {
			t.Errorf("clean exact private probe directory: %v", runtimes.CommandError("remove probe directory", output, err))
		}
	})
	for _, name := range []string{"ca.pem", "client.pem", "client-key.pem"} {
		data, err := os.ReadFile(filepath.Join(manager.instancePath(instance.Name), "tls", name))
		if err != nil {
			t.Fatal(err)
		}
		if output, err := run(ctx, data, "sh", "-c", "umask 077; cat > "+directory+"/"+name); err != nil {
			t.Fatal(runtimes.CommandError("stage only a synthetic probe certificate", output, err))
		}
	}
	curl := []string{"curl", "--fail-with-body", "--silent", "--show-error", "--connect-timeout", "5", "--max-time", "10",
		"--cacert", directory + "/ca.pem", "--cert", directory + "/client.pem", "--key", directory + "/client-key.pem"}
	endpoint := instance.Members[0].ClientURL
	if output, err := run(ctx, nil, append(append([]string(nil), curl...), endpoint+"/readyz")...); err != nil {
		t.Fatal(runtimes.CommandError("verify host.lima.internal mTLS readiness from the authorized VM", output, err))
	}
	output, err := run(ctx, nil, append(append([]string(nil), curl...), endpoint+discoveryPath)...)
	if err != nil {
		t.Fatal(runtimes.CommandError("verify synthetic endpoint identity from the authorized VM", output, err))
	}
	var discovery Discovery
	if err := json.Unmarshal(output, &discovery); err != nil {
		t.Fatal(err)
	}
	if discovery.ClusterID != instance.ClusterID || discovery.MemberID != instance.Members[0].ID {
		t.Fatal("guest DNS/routing reached a different datastore identity")
	}
	t.Log(fmt.Sprintf("verified guest %s reaches its synthetic host.lima.internal endpoint over mTLS with the expected identity", machine))
}
