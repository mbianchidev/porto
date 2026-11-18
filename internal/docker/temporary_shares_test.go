package docker

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestMissingTemporarySharesKeepsExistingMounts(t *testing.T) {
	configuration := []byte("mounts:\n- location: \"~\"\n  writable: true\n- location: /private/tmp\n")
	missing, err := missingTemporaryShares(configuration, macOSTemporaryShares)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(missing, []string{"/private/var/folders"}) {
		t.Fatalf("missing shares = %q", missing)
	}
	expression, err := temporaryShareExpression(missing)
	if err != nil || !strings.HasPrefix(expression, ".mounts = (.mounts // []) + ") {
		t.Fatalf("share expression = %q, %v; want an append that keeps the home mount", expression, err)
	}
}

func TestTranslateTemporaryMountsTargetsSharedPrivateRoots(t *testing.T) {
	guarded := 0
	manager := &Manager{temporaryShares: macOSTemporaryShares, temporaryShareGuard: func(context.Context) error {
		guarded++
		return nil
	}}
	binds, mounts, err := manager.translateTemporaryMounts(context.Background(),
		[]string{"/tmp/work:/w:ro", "/var/folders/ab/T/tmp.1:/t", "/Users/example/app:/app", "named:/data", "/tmpfiles:/x"},
		[]dockerStructuredMount{{Type: "bind", Source: "/tmp"}, {Type: "volume", Source: "/tmp/not-a-path"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	wantBinds := []string{"/private/tmp/work:/w:ro", "/private/var/folders/ab/T/tmp.1:/t", "/Users/example/app:/app", "named:/data", "/tmpfiles:/x"}
	if !reflect.DeepEqual(binds, wantBinds) || mounts[0].Source != "/private/tmp" || mounts[1].Source != "/tmp/not-a-path" {
		t.Fatalf("binds = %q, mounts = %+v", binds, mounts)
	}
	if guarded != 1 {
		t.Fatalf("share guard ran %d times, want once", guarded)
	}

	manager.temporaryShareGuard = func(context.Context) error { return errors.New("not shared yet") }
	if _, _, err := manager.translateTemporaryMounts(context.Background(), []string{"/Users/example/app:/app"}, nil); err != nil {
		t.Fatalf("home binds consulted the temporary share guard: %v", err)
	}
	if _, _, err := manager.translateTemporaryMounts(context.Background(), []string{"/tmp/work:/w"}, nil); err == nil {
		t.Fatal("a temporary bind was accepted although the engine does not share it")
	}
}
