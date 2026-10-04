package datafiles

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestManagedArchivePathsRejectOutsideRootsAndSymlinkEscapes(t *testing.T) {
	state := t.TempDir()
	t.Setenv("PORTO_HOME", state)
	transfers := filepath.Join(state, "transfers")
	if err := os.Mkdir(transfers, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, outside := range []string{
		filepath.Join(t.TempDir(), "outside.tar"),
		filepath.Join(state, "porto.db"),
		filepath.Join(state, "transfers", "..", "..", "outside.tar"),
	} {
		if _, _, err := ManagedLocation(outside); err == nil {
			t.Fatalf("unmanaged path accepted: %s", outside)
		}
	}
	if runtime.GOOS == "windows" {
		t.Skip("host symlink creation requires Windows privileges")
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "fixture.tar"), []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(transfers, "escape")); err != nil {
		t.Fatal(err)
	}
	if root, file, err := OpenManagedArchive(filepath.Join(transfers, "escape", "fixture.tar")); err == nil {
		file.Close()
		root.Close()
		t.Fatal("archive symlink escaped the managed root")
	}
}
