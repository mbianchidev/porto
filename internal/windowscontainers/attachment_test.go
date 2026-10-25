package windowscontainers

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNativeWindowsContainersCNIUsesExactNerdctlNamespacePath(t *testing.T) {
	i := Installation{Root: t.TempDir(), OwnerSID: "S-1-5-21-111-222-333-1001"}
	name := i.DefaultNetwork()
	path := filepath.Join(i.Data(), "cni", Namespace, "nerdctl-"+name+".conflist")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	valid := `{"cniVersion":"1.0.0","name":"` + name + `","plugins":[{"type":"nat","ipam":{"subnet":"10.253.1.0/24"}}]}`
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	// The non-namespaced location is not a candidate for an owned network.
	if err := os.WriteFile(filepath.Join(i.Data(), "cni", "nerdctl-"+name+".conflist"), []byte("malformed decoy"), 0o600); err != nil {
		t.Fatal(err)
	}
	var checked []string
	data, err := readOwnedCNIConfiguration(i, name, func(path string) error { checked = append(checked, path); return nil })
	if err != nil || string(data) != valid {
		t.Fatalf("exact native CNI read=%s, %v", data, err)
	}
	if !reflect.DeepEqual(checked, []string{filepath.Dir(path), path}) {
		t.Fatalf("CNI ownership checked wrong paths: %v", checked)
	}
	for _, body := range []string{`{`, `null`, `{"name":"foreign","plugins":[{"type":"nat"}]}`,
		`{"name":"` + name + `","plugins":[{"type":"bridge"}]}`, `{"name":"` + name + `","plugins":[{"type":"nat","ipam":{"type":"host-local"}}]}`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readOwnedCNIConfiguration(i, name, func(string) error { return nil }); err == nil {
			t.Fatalf("malformed owned CNI was ignored: %s", body)
		}
	}
	denied := errors.New("synthetic protected path denial")
	if _, err := readOwnedCNIConfiguration(i, name, func(string) error { return denied }); !errors.Is(err, denied) {
		t.Fatalf("CNI protection error=%v", err)
	}
	if _, err := ownedCNIPath(i, "../foreign"); err == nil || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("unsafe native CNI name=%v", err)
	}
}
