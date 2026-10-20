package nativefiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestMacOSNativeWebDAVOwnerProcess(t *testing.T) {
	if os.Getenv("PORTO_NATIVE_WEBDAV_OWNER") != "1" {
		t.Skip("isolated native owner subprocess")
	}
	resource := datafiles.Resource{Kind: "volume", Name: "synthetic-crash", ID: "synthetic-crash-volume"}
	target := Target{
		Resource: resource, Identity: resource.Fingerprint(),
		SFTP:   &SFTPCommand{Name: os.Args[0], Args: []string{"-test.run=^TestNativeFixtureServerProcess$"}, Env: []string{"PORTO_NATIVE_SYNTHETIC_SFTP=" + os.Getenv("PORTO_NATIVE_WEBDAV_SOURCE")}},
		Verify: func(context.Context) error { return nil }, Release: func(context.Context) error { return nil },
	}
	manager := New(os.Getenv("PORTO_NATIVE_WEBDAV_ROOT"), nil)
	record, err := manager.Attach(context.Background(), target, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := json.NewEncoder(os.Stdout).Encode(record); err != nil {
		os.Exit(2)
	}
	var input [1]byte
	_, _ = os.Stdin.Read(input[:])
	os.Exit(0)
}

func TestMacOSNativeWebDAVRecoversAnActualInterruptedOwner(t *testing.T) {
	if os.Getenv("PORTO_TEST_WEBDAV_NATIVE") != "1" {
		t.Skip("requires real macOS mounts")
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "synthetic.txt"), []byte("original source"), 0o640); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "native owner restart")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	owner, err := (runtimes.ExecRunner{}).Start(ctx, runtimes.Command{
		Name: os.Args[0], Args: []string{"-test.run=^TestMacOSNativeWebDAVOwnerProcess$"},
		Env: []string{"PORTO_NATIVE_WEBDAV_OWNER=1", "PORTO_NATIVE_WEBDAV_SOURCE=" + source, "PORTO_NATIVE_WEBDAV_ROOT=" + root},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Kill()
	var record Attachment
	if err := json.NewDecoder(owner.Stdout()).Decode(&record); err != nil {
		diagnostics, _ := io.ReadAll(owner.Stderr())
		t.Fatalf("owned native process never mounted: %v: %s", err, diagnostics)
	}
	if content, err := os.ReadFile(filepath.Join(record.Path, "synthetic.txt")); err != nil || string(content) != "original source" {
		t.Fatalf("owned host mount: %q, %v", content, err)
	}
	if err := owner.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Wait(); err == nil {
		t.Fatal("native owner did not actually terminate")
	}
	restarted := New(root, nil)
	if err := restarted.Recover(ctx, func(context.Context, Attachment) error {
		return errors.New("a direct SFTP mount has no persistent backend bind to release")
	}); err != nil {
		t.Fatal(err)
	}
	if len(restarted.List()) != 0 {
		t.Fatal("interrupted owner recovery kept a dead attachment")
	}
	if _, err := os.Lstat(record.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dead mount path survived recovery: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(source, "synthetic.txt")); err != nil || string(content) != "original source" {
		t.Fatalf("recovery changed source data: %q, %v", content, err)
	}
}

type nativeDetachFailureRunner struct {
	runtimes.ExecRunner
	fail bool
}

func (runner *nativeDetachFailureRunner) Run(ctx context.Context, command runtimes.Command) ([]byte, error) {
	if runner.fail && command.Name == "umount" {
		return nil, os.ErrPermission
	}
	return runner.ExecRunner.Run(ctx, command)
}

func TestMacOSNativeWebDAVFailedDetachRetainsItsOwnedMountForRetry(t *testing.T) {
	if os.Getenv("PORTO_TEST_WEBDAV_NATIVE") != "1" {
		t.Skip("requires real macOS mounts")
	}
	target, source := syntheticNativeTarget(t, "volume")
	runner := &nativeDetachFailureRunner{}
	manager := New(t.TempDir(), runner)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	record, err := manager.Attach(ctx, target, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		runner.fail = false
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	runner.fail = true
	if err := manager.Detach(ctx, record.ID); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("failed detach was hidden: %v", err)
	}
	records := manager.List()
	if len(records) != 1 || records[0].State != "detach-failed" {
		t.Fatalf("failed cleanup lost its owned record: %+v", records)
	}
	if _, err := manager.Get(ctx, record.ID); err == nil {
		t.Fatal("failed detach still advertised connected access")
	}
	runner.fail = false
	if err := manager.Detach(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(source, "synthetic.txt")); err != nil || string(content) != "synthetic-original" {
		t.Fatalf("failed detach damaged source files: %q, %v", content, err)
	}
}

func TestMacOSNativeWebDAVMountsActualReadonlyAndWritableHostFolders(t *testing.T) {
	if os.Getenv("PORTO_TEST_WEBDAV_NATIVE") != "1" {
		t.Skip("set PORTO_TEST_WEBDAV_NATIVE=1 for real macOS native filesystem mounts")
	}
	for _, kind := range []string{"container", "image", "volume", "vm"} {
		t.Run(kind, func(t *testing.T) {
			target, source := syntheticNativeTarget(t, kind)
			manager := New(filepath.Join(t.TempDir(), "Porto native \u03b4 paths"), nil)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := manager.Close(cleanup); err != nil {
					t.Error(err)
				}
			})
			attachment, err := manager.Attach(ctx, target, kind != "image")
			if err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(filepath.Join(attachment.Path, "synthetic.txt"))
			if err != nil || string(content) != "synthetic-original" {
				t.Fatalf("real host folder did not expose resource files: %q, %v", content, err)
			}
			if kind == "image" {
				if err := os.WriteFile(filepath.Join(attachment.Path, "synthetic.txt"), []byte("forbidden"), 0o600); err == nil {
					t.Fatal("the macOS filesystem allowed an image write")
				}
				if _, err := manager.Attach(ctx, target, true); !errors.Is(err, datafiles.ErrUnsupported) {
					t.Fatalf("image write attachment: %v", err)
				}
			} else {
				path := filepath.Join(attachment.Path, "editor-\u03b4.tmp")
				if err := os.WriteFile(path, []byte("saved native edit"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path, filepath.Join(attachment.Path, "saved.txt")); err != nil {
					t.Fatal(err)
				}
				content, err := os.ReadFile(filepath.Join(source, "saved.txt"))
				if err != nil || string(content) != "saved native edit" {
					t.Fatalf("native save did not reach workload: %q, %v", content, err)
				}
				if err := os.WriteFile(filepath.Join(source, "guest-created.txt"), []byte("guest change"), 0o600); err != nil {
					t.Fatal(err)
				}
				content, err = os.ReadFile(filepath.Join(attachment.Path, "guest-created.txt"))
				if err != nil || string(content) != "guest change" {
					t.Fatalf("guest change was not accessible to the native client: %q, %v", content, err)
				}
			}
			if _, err := manager.Get(ctx, attachment.ID); err != nil {
				t.Fatal(err)
			}
			if err := manager.Detach(ctx, attachment.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(attachment.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("native detach left the host path: %v", err)
			}
			if content, err := os.ReadFile(filepath.Join(source, "synthetic.txt")); err != nil || string(content) != "synthetic-original" {
				t.Fatalf("native cleanup changed source data: %q, %v", content, err)
			}
		})
	}
}
