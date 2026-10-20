package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func claimFreshClientLima(t *testing.T, ctx context.Context, manager *Manager) {
	t.Helper()
	limaHome := filepath.Clean(os.Getenv("LIMA_HOME"))
	runnerTemporary := filepath.Clean(os.Getenv("RUNNER_TEMP"))
	relative, err := filepath.Rel(runnerTemporary, limaHome)
	if os.Getenv("GITHUB_ACTIONS") != "true" || !filepath.IsAbs(limaHome) || !filepath.IsAbs(runnerTemporary) ||
		err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		t.Fatal("fresh Lima fixture ownership is restricted to an isolated CI runtime")
	}
	if _, err := os.Stat(manager.engineStatePath()); !os.IsNotExist(err) {
		t.Fatal("refusing to replace an existing engine ownership record")
	}
	helper, err := manager.runtimeHelperPath()
	if err != nil || helper == "" {
		t.Fatalf("fresh Lima fixture requires the bundled Linux runtime helper: path=%q, error=%v", helper, err)
	}
	owner := newInventoryInstanceID()
	if err := manager.writeLimaOwnership(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := manager.writeEngineState(engineState{Mode: "lima", Instance: engineInstanceName, OwnerID: owner, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := manager.installLimaRuntimeHelper(ctx, engineInstanceName); err != nil {
		t.Fatal(err)
	}
}
