package docker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestCancelledVolumeCreationCleansOnlyProvenTemporaryResources(t *testing.T) {
	for _, owned := range []bool{true, false} {
		t.Run(map[bool]string{true: "owned", false: "foreign"}[owned], func(t *testing.T) {
			state := t.TempDir()
			t.Setenv("PORTO_HOME", state)
			source := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "fixture.txt"), []byte("synthetic"), 0o600); err != nil {
				t.Fatal(err)
			}
			archiveDirectory := filepath.Join(state, "transfers")
			if err := os.Mkdir(archiveDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			archivePath := filepath.Join(archiveDirectory, "fixture.tar")
			file, err := os.Create(archivePath)
			if err != nil {
				t.Fatal(err)
			}
			_, exportErr := datafiles.Export(context.Background(), file, source, datafiles.Resource{Kind: "volume", Name: "fixture", ID: "fixture-source"})
			if err := errors.Join(exportErr, file.Close()); err != nil {
				t.Fatal(err)
			}
			archive, err := InspectVolumeArchive(context.Background(), archivePath)
			if err != nil {
				t.Fatal(err)
			}
			owner := ""
			removed := false
			runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
				args := strings.Join(command.Args, " ")
				switch {
				case strings.HasPrefix(args, "volume create "):
					for _, value := range command.Args {
						if strings.HasPrefix(value, transferOwnerLabel+"=") {
							owner = strings.TrimPrefix(value, transferOwnerLabel+"=")
						}
					}
					return nil, context.Canceled
				case args == "volume inspect restored-fixture":
					if !owned {
						owner = "foreign-owner"
					}
					return json.Marshal([]map[string]any{{"Name": "restored-fixture", "Labels": map[string]string{transferOwnerLabel: owner}}})
				case args == "volume rm restored-fixture":
					removed = true
					return nil, nil
				default:
					return nil, nil
				}
			}}
			_, err = New(runner).ImportVolume(context.Background(), "restored-fixture", archivePath, archive.SHA256, func(string, int64) error { return nil })
			if !errors.Is(err, context.Canceled) || removed != owned {
				t.Fatalf("cancelled import error=%v removed=%v expected=%v", err, removed, owned)
			}
		})
	}
}
