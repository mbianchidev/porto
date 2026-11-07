package etcqlite

import (
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
