package etcqlite

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"testing"
)

func TestNativeStatePathsUseInertIdentityTokens(t *testing.T) {
	manager := New(t.TempDir(), nil)
	token := regexp.MustCompile(`^[0-9a-f]{24}$`)
	for _, name := range []string{"synthetic", "../outside", "/absolute/foreign", "name\x00with-null", `..\foreign`} {
		for _, path := range []string{manager.instancePath(name), manager.retainedPath(name, "../foreign-cluster")} {
			relative, err := filepath.Rel(manager.root, path)
			if err != nil || !filepath.IsLocal(relative) || !token.MatchString(filepath.Base(path)) {
				t.Fatalf("user text became a native filesystem component: %q -> %q (%v)", name, path, err)
			}
		}
	}
	if manager.instancePath("first") == manager.instancePath("second") ||
		manager.retainedPath("first", "1") == manager.retainedPath("first", "2") {
		t.Fatal("distinct native resource identities used the same state token")
	}
}

func TestLogStreamsSelectOnlyFixedOwnedFiles(t *testing.T) {
	manager, _ := fixtureManager(t)
	instance, err := manager.Create(context.Background(), CreateRequest{Name: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range []string{"stdout", "stderr"} {
		if err := writeExclusive(filepath.Join(manager.memberPath(instance, instance.Members[0]), stream+".log"), []byte(stream+" synthetic line\n")); err != nil {
			t.Fatal(err)
		}
		lines, err := manager.Logs(context.Background(), instance.Name, stream, 10)
		if err != nil || len(lines) != 1 || lines[0] != "member-1: "+stream+" synthetic line" {
			t.Fatalf("fixed log stream %s: %v %v", stream, lines, err)
		}
	}
	if _, err := manager.Logs(context.Background(), instance.Name, "../metadata", 10); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unlisted log path was accepted: %v", err)
	}
}
