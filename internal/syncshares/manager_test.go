package syncshares

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type syntheticProvider struct {
	mu    sync.Mutex
	owner string
	beta  string
}

func (p *syntheticProvider) Endpoint(context.Context, string, bool) (Endpoint, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Endpoint{OwnerID: p.owner, URL: p.beta, GuestPath: p.beta}, nil
}
func (*syntheticProvider) InUse(context.Context, string) error { return nil }

type syntheticSyncRunner struct {
	mu      sync.Mutex
	version string
	session Session
	flushes int
}

func (r *syntheticSyncRunner) Run(_ context.Context, command runtimes.Command) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	args := command.Args
	if len(args) == 1 && args[0] == "version" {
		return []byte(r.version), nil
	}
	if len(args) < 2 || args[0] != "sync" {
		return nil, errors.New("unexpected synthetic synchronization command")
	}
	switch args[1] {
	case "create":
		r.session = Session{Identifier: "sync_synthetic", Mode: "two-way-safe", Status: "watching", Labels: map[string]string{},
			Alpha: EndpointState{Protocol: "local", Path: args[len(args)-2], Connected: true, Scanned: true},
			Beta:  EndpointState{Protocol: "local", Path: args[len(args)-1], Connected: true, Scanned: true}}
		for _, argument := range args {
			if name, ok := strings.CutPrefix(argument, "--name="); ok {
				r.session.Name = name
			}
			if label, ok := strings.CutPrefix(argument, "--label="); ok {
				key, value, _ := strings.Cut(label, "=")
				r.session.Labels[key] = value
			}
		}
		return []byte("Created session " + r.session.Identifier), nil
	case "list":
		return json.Marshal([]Session{r.session})
	case "flush":
		r.flushes++
	case "pause":
		r.session.Paused = true
	case "resume":
		r.session.Paused = false
	case "terminate":
		r.session = Session{}
	}
	return nil, nil
}

func syncFixture(t *testing.T) (*Manager, *syntheticProvider, *syntheticSyncRunner, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("native synchronized shares are capability-gated to POSIX hosts")
	}
	root, err := os.MkdirTemp("/tmp", "porto-sync-unit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	for _, name := range []string{"alpha", "beta", "config"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	provider := &syntheticProvider{owner: "synthetic-owner", beta: filepath.Join(root, "beta")}
	runner := &syntheticSyncRunner{version: supportedVersion}
	manager := New(filepath.Join(root, "config"), runner, provider)
	manager.goos = "linux"
	manager.lookPath = func(name string) (string, error) { return name, nil }
	return manager, provider, runner, filepath.Join(root, "alpha")
}

func TestSynchronizedSharesRejectUnsafeToolsAndOverlappingRoots(t *testing.T) {
	manager, _, runner, root := syncFixture(t)
	runner.version = "0.18.1"
	if _, err := manager.Create(context.Background(), 1, root, nil); err == nil {
		t.Fatal("an older vulnerable upstream toolchain was accepted")
	}
	runner.version = supportedVersion
	if _, err := manager.Create(context.Background(), 1, root, []string{"node_modules"}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(context.Background(), 1, root, nil); err == nil {
		t.Fatal("overlapping synchronized roots were accepted")
	}
}

func TestSynchronizedShareMappingVerifiesReadinessAndOwnershipBeforeTransfer(t *testing.T) {
	manager, provider, runner, root := syncFixture(t)
	share, err := manager.Create(context.Background(), 1, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := manager.Resolve(context.Background(), canonical)
	if err != nil || mapped != provider.beta {
		t.Fatalf("selected bind mapping = %q, %v", mapped, err)
	}
	runner.mu.Lock()
	before := runner.flushes
	runner.mu.Unlock()
	provider.mu.Lock()
	provider.owner = "replacement-synthetic-owner"
	provider.mu.Unlock()
	if _, err := manager.Resolve(context.Background(), canonical); err == nil {
		t.Fatal("a replacement engine adopted the original synchronized share")
	}
	runner.mu.Lock()
	if runner.flushes != before {
		t.Error("ownership was checked only after transferring files")
	}
	runner.mu.Unlock()
	provider.mu.Lock()
	provider.owner = share.OwnerID
	provider.mu.Unlock()
	runner.mu.Lock()
	runner.session.Conflicts = []json.RawMessage{json.RawMessage(`{"synthetic":"both sides changed"}`)}
	runner.mu.Unlock()
	if _, err := manager.Resolve(context.Background(), canonical); err == nil {
		t.Fatal("a conflicting share silently became a bind source")
	}
	if err := manager.SetMode(context.Background(), share.ID, "bind"); err != nil {
		t.Fatal(err)
	}
	ordinary, err := manager.Resolve(context.Background(), canonical)
	if err != nil || ordinary != canonical {
		t.Fatalf("ordinary bind option was not preserved: %q, %v", ordinary, err)
	}
}

func TestSynchronizedSharesRejectRootsAndEscapingSymlinks(t *testing.T) {
	manager, _, _, root := syncFixture(t)
	if _, err := manager.Create(context.Background(), 1, string(filepath.Separator), nil); err == nil {
		t.Fatal("filesystem-wide synchronization was accepted")
	}
	if _, err := manager.Create(context.Background(), 1, root, nil); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(root), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Resolve(context.Background(), filepath.Join(canonical, "escape")); err == nil {
		t.Fatal("a bind source escaped its explicitly selected synchronized root")
	}
}

func TestSynchronizedSharesRejectRecreatedHostRootsBeforeTransferring(t *testing.T) {
	manager, _, runner, root := syncFixture(t)
	if _, err := manager.Create(context.Background(), 1, root, nil); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	before := runner.flushes
	runner.mu.Unlock()
	if _, err := manager.Resolve(context.Background(), canonical); err == nil {
		t.Fatal("a recreated host root was silently synchronized")
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.flushes != before {
		t.Fatal("the recreated host root was transferred before its identity was checked")
	}
}

func TestSynchronizedSharesMapParentAliasesWithoutAllowingEscapingSources(t *testing.T) {
	manager, provider, _, root := syncFixture(t)
	if _, err := manager.Create(context.Background(), 1, root, nil); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(root), "parent-alias")
	if err := os.Symlink(filepath.Dir(root), alias); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(alias, filepath.Base(root))
	mapped, err := manager.Resolve(context.Background(), source)
	if err != nil || mapped != provider.beta {
		t.Fatalf("a parent alias bypassed its selected share: %q, %v", mapped, err)
	}
	outside := filepath.Join(filepath.Dir(root), "unselected")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Resolve(context.Background(), filepath.Join(source, "escape")); err == nil {
		t.Fatal("a parent alias let a source escape its selected root")
	}
}
