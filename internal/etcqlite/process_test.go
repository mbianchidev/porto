package etcqlite

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNativeProcessIdentityIncludesBirthAndExecutable(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("native identity profile")
	}
	identity, err := inspectPID(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if identity.PID != os.Getpid() || identity.Birth == "" || identity.Executable != path {
		t.Fatalf("process ownership proof is incomplete: %+v, executable=%s", identity, path)
	}
}
