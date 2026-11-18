package docker

import (
	"bytes"
	"encoding/xml"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestEndpointLaunchDaemonRestoresOnlyAMissingLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the restore job runs under /bin/sh")
	}
	directory := t.TempDir()
	target := filepath.Join(directory, "Application Support & <porto>", "docker.sock")
	canonical := filepath.Join(directory, "docker.sock")
	plist, err := endpointLaunchDaemon(canonical, target)
	if err != nil {
		t.Fatal(err)
	}
	values := plistStrings(t, plist)
	if len(values) < 2 || values[1] != EndpointLaunchDaemonLabel {
		t.Fatalf("plist strings = %q", values)
	}
	arguments := values[2:]
	if !reflect.DeepEqual(arguments, endpointRestoreArguments(canonical, target)) {
		t.Fatalf("plist arguments = %q", arguments)
	}
	restore := func() {
		t.Helper()
		if output, err := exec.Command(arguments[0], arguments[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("restore job failed: %v: %s", err, output)
		}
	}

	restore()
	if link, err := os.Readlink(canonical); err != nil || link != target {
		t.Fatalf("restored link = %q, %v; want %q", link, err, target)
	}

	other := filepath.Join(directory, "other.sock")
	if err := os.Remove(canonical); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, canonical); err != nil {
		t.Fatal(err)
	}
	restore()
	if link, err := os.Readlink(canonical); err != nil || link != other {
		t.Fatalf("restore replaced another runtime's link: %q, %v", link, err)
	}

	if err := os.Remove(canonical); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canonical, []byte("socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	restore()
	if info, err := os.Lstat(canonical); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("restore replaced a non-link endpoint: %v, %v", info, err)
	}
}

func TestEndpointLaunchDaemonRejectsUnsafePaths(t *testing.T) {
	for _, paths := range [][2]string{{"docker.sock", "/tmp/porto.sock"}, {"/var/run/docker.sock", "/tmp/porto\n.sock"}} {
		if _, err := endpointLaunchDaemon(paths[0], paths[1]); err == nil {
			t.Fatalf("endpointLaunchDaemon(%q, %q) accepted an unsafe path", paths[0], paths[1])
		}
	}
}

func plistStrings(t *testing.T, plist []byte) []string {
	t.Helper()
	decoder := xml.NewDecoder(bytes.NewReader(plist))
	decoder.Strict = true
	var values []string
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return values
		}
		if err != nil {
			t.Fatalf("invalid plist XML: %v\n%s", err, plist)
		}
		if start, ok := token.(xml.StartElement); ok && start.Name.Local == "string" {
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				t.Fatal(err)
			}
			values = append(values, value)
		}
	}
}
