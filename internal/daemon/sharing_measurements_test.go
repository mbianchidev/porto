package daemon

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	portodocker "github.com/mbianchidev/porto/internal/docker"
)

func writeSyntheticSharingPayload(t *testing.T, root string) {
	t.Helper()
	random := rand.NewPCG(1, 2)
	for index := range 8 {
		data := make([]byte, 8<<20)
		for offset := 0; offset < len(data); offset += 8 {
			binary.LittleEndian.PutUint64(data[offset:], random.Uint64())
		}
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("payload-%02d.bin", index)), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func measureOwnedSharingModes(t *testing.T, ctx context.Context, manager *portodocker.Manager, image, architecture, root string) {
	t.Helper()
	var originalDigest string
	for _, mode := range []string{"bind", "synchronized"} {
		labels := map[string]string{}
		if mode == "bind" {
			labels["dev.porto.share-mode"] = "bind"
		}
		id, err := manager.RunContainer(ctx, portodocker.CreateContainerRequest{
			Name:  fmt.Sprintf("porto-share-measurement-%s-%d", mode, time.Now().UnixNano()),
			Image: image, Platform: "linux/" + architecture, Volumes: []string{root + ":/workspace:ro"}, Labels: labels,
		})
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if err := manager.ContainerAction(cleanup, id, "remove-force"); err != nil {
					t.Error(err)
				}
			}()
			for trial := range 3 {
				output, err := manager.ExecContainer(ctx, id, []string{"/fixture", "--bench"}, nil)
				if err != nil {
					t.Fatalf("actual %s bind workload: %v: %s", mode, err, output)
				}
				var measured struct {
					Files, Bytes, Nanoseconds int64
					SHA256                    string
				}
				if err := json.Unmarshal(output, &measured); err != nil {
					t.Fatalf("decode %s benchmark: %v: %s", mode, err, output)
				}
				if measured.Files < 10009 || measured.Bytes != 64<<20 || measured.Nanoseconds <= 0 || len(measured.SHA256) != 64 {
					t.Fatalf("benchmark did not traverse the complete identical dataset: %+v", measured)
				}
				if originalDigest == "" {
					originalDigest = measured.SHA256
				}
				if measured.SHA256 != originalDigest {
					t.Fatal("ordinary and synchronized binds exposed different dataset bytes")
				}
				elapsed := time.Duration(measured.Nanoseconds)
				t.Logf("representative sharing mode=%s trial=%d files=%d payload=%d bytes walk+read=%s effective=%.3f MiB/s SHA256=%s; subsequent trials may be warm",
					mode, trial+1, measured.Files, measured.Bytes, elapsed,
					float64(measured.Bytes)/(1<<20)/elapsed.Seconds(), measured.SHA256)
			}
		}()
	}
}
