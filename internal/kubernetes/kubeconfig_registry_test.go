package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/vm"
	"gopkg.in/yaml.v3"
)

func TestKubeconfigRegistryRegistersWithoutChangingCurrentContext(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, ".kube", "config")
	source := filepath.Join(dir, "porto.yaml")
	writeTestKubeconfig(t, target, `apiVersion: v1
kind: Config
current-context: existing
preferences:
  colors: true
extensions:
  - name: external.example/settings
    extension:
      enabled: true
clusters:
  - name: existing
    cluster:
      server: https://existing.example
contexts:
  - name: existing
    context:
      cluster: existing
      user: existing
users:
  - name: existing
    user:
      token: existing-token
`)
	writeTestKubeconfig(t, source, `apiVersion: v1
kind: Config
current-context: porto-dev
clusters:
  - name: porto-dev
    cluster:
      certificate-authority-data: Y2E=
      server: https://127.0.0.1:6443
contexts:
  - name: porto-dev
    context:
      cluster: porto-dev
      user: porto-dev
users:
  - name: porto-dev
    user:
      client-certificate-data: Y2VydA==
      client-key-data: a2V5
`)

	registration, err := NewKubeconfigRegistry(target).Register(context.Background(), source)
	if err != nil {
		t.Fatalf("register kubeconfig: %v", err)
	}
	if registration.Context != "porto-dev" || registration.Path != target {
		t.Fatalf("registration = %+v", registration)
	}

	document := readTestKubeconfig(t, target)
	if got := document["current-context"]; got != "existing" {
		t.Fatalf("current context = %#v, want existing", got)
	}
	if testNamedEntry(t, document, "contexts", "existing") == nil {
		t.Fatal("existing context was removed")
	}
	portoContext := testNamedEntry(t, document, "contexts", "porto-dev")
	contextValue := testMap(t, portoContext["context"])
	if !testHasManagedExtension(t, contextValue) {
		t.Fatal("Porto context was not marked as managed")
	}
	if testNamedEntry(t, document, "clusters", "porto-dev") == nil {
		t.Fatal("Porto cluster was not registered")
	}
	if testNamedEntry(t, document, "users", "porto-dev") == nil {
		t.Fatal("Porto user was not registered")
	}
	preferences := testMap(t, document["preferences"])
	if preferences["colors"] != true {
		t.Fatalf("global preferences changed: %#v", preferences)
	}
	extensions, ok := document["extensions"].([]any)
	if !ok || len(extensions) != 1 {
		t.Fatalf("global extensions changed: %#v", document["extensions"])
	}
	externalExtension := testMap(t, extensions[0])
	if externalExtension["name"] != "external.example/settings" {
		t.Fatalf("global extension changed: %#v", externalExtension)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("kubeconfig mode = %o, want 600", info.Mode().Perm())
		}
	}
}

func TestKubeconfigRegistryInitializesEmptyTargetWithoutSelectingContext(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config")
	source := filepath.Join(dir, "porto.yaml")
	writeTestKubeconfig(t, target, "")
	writeTestKubeconfig(t, source, `apiVersion: v1
kind: Config
current-context: porto-dev
clusters:
  - name: porto-dev
    cluster:
      server: https://127.0.0.1:6443
contexts:
  - name: porto-dev
    context:
      cluster: porto-dev
      user: porto-dev
users:
  - name: porto-dev
    user:
      token: porto-token
`)

	if _, err := NewKubeconfigRegistry(target).Register(context.Background(), source); err != nil {
		t.Fatalf("register into empty kubeconfig: %v", err)
	}
	document := readTestKubeconfig(t, target)
	if got := document["current-context"]; got != "" {
		t.Fatalf("current context = %#v, want empty", got)
	}
}

func TestKubeconfigRegistryRejectsUnmanagedContextCollision(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config")
	source := filepath.Join(dir, "porto.yaml")
	original := `apiVersion: v1
kind: Config
current-context: porto-dev
clusters:
  - name: porto-dev
    cluster:
      server: https://unrelated.example
contexts:
  - name: porto-dev
    context:
      cluster: porto-dev
      user: porto-dev
users:
  - name: porto-dev
    user:
      token: unrelated-token
`
	writeTestKubeconfig(t, target, original)
	writeTestKubeconfig(t, source, `apiVersion: v1
kind: Config
current-context: porto-dev
clusters:
  - name: porto-dev
    cluster:
      server: https://127.0.0.1:6443
contexts:
  - name: porto-dev
    context:
      cluster: porto-dev
      user: porto-dev
users:
  - name: porto-dev
    user:
      token: porto-token
`)

	if _, err := NewKubeconfigRegistry(target).Register(context.Background(), source); err == nil {
		t.Fatal("unmanaged context collision was overwritten")
	}
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != original {
		t.Fatalf("colliding kubeconfig changed:\n%s", contents)
	}
}

func TestKubeconfigRegistryAdoptsMatchingLegacyContext(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config")
	source := filepath.Join(dir, "porto.yaml")
	config := `apiVersion: v1
kind: Config
current-context: porto-k3s-dev
clusters:
  - name: porto-k3s-dev
    cluster:
      certificate-authority-data: Y2E=
      server: https://127.0.0.1:54321
contexts:
  - name: porto-k3s-dev
    context:
      cluster: porto-k3s-dev
      user: porto-k3s-dev
users:
  - name: porto-k3s-dev
    user:
      client-certificate-data: Y2VydA==
      client-key-data: a2V5
`
	writeTestKubeconfig(t, target, config)
	writeTestKubeconfig(t, source, config)

	if _, err := NewKubeconfigRegistry(target).Register(context.Background(), source); err != nil {
		t.Fatalf("adopt matching context: %v", err)
	}
	document := readTestKubeconfig(t, target)
	contextEntry := testNamedEntry(t, document, "contexts", "porto-k3s-dev")
	if !testHasManagedExtension(t, testMap(t, contextEntry["context"])) {
		t.Fatal("matching legacy context was not marked as managed")
	}
}

func TestKubeconfigRegistryRejectsReferencedClusterCollision(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config")
	source := filepath.Join(dir, "porto.yaml")
	writeTestKubeconfig(t, target, `apiVersion: v1
kind: Config
current-context: shared
clusters:
  - name: porto-dev
    cluster:
      server: https://shared.example
contexts:
  - name: shared
    context:
      cluster: porto-dev
      user: shared
users:
  - name: shared
    user:
      token: shared-token
`)
	writeTestKubeconfig(t, source, `apiVersion: v1
kind: Config
current-context: porto-dev
clusters:
  - name: porto-dev
    cluster:
      server: https://127.0.0.1:6443
contexts:
  - name: porto-dev
    context:
      cluster: porto-dev
      user: porto-dev
users:
  - name: porto-dev
    user:
      token: porto-token
`)

	if _, err := NewKubeconfigRegistry(target).Register(context.Background(), source); err == nil {
		t.Fatal("referenced cluster collision was overwritten")
	}
	document := readTestKubeconfig(t, target)
	cluster := testMap(t, testNamedEntry(t, document, "clusters", "porto-dev")["cluster"])
	if cluster["server"] != "https://shared.example" {
		t.Fatalf("shared cluster was changed: %#v", cluster)
	}
}

func TestKubeconfigRegistryRefreshesManagedBundleReferencedByExternalAlias(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config")
	source := filepath.Join(dir, "porto.yaml")
	writeTestKubeconfig(t, source, `apiVersion: v1
kind: Config
current-context: porto-dev
clusters:
  - name: porto-dev
    cluster:
      server: https://127.0.0.1:54321
contexts:
  - name: porto-dev
    context:
      cluster: porto-dev
      user: porto-dev
users:
  - name: porto-dev
    user:
      token: old-token
`)
	registry := NewKubeconfigRegistry(target)
	owner := KubeconfigOwner{Cluster: "dev", Provider: "kind"}
	if _, err := registry.Register(context.Background(), source, owner); err != nil {
		t.Fatal(err)
	}
	document := readTestKubeconfig(t, target)
	document["current-context"] = "dev-alias"
	document["contexts"] = append(document["contexts"].([]any), map[string]any{
		"name": "dev-alias",
		"context": map[string]any{
			"cluster":   "porto-dev",
			"user":      "porto-dev",
			"namespace": "external",
		},
	})
	contents, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	writeTestKubeconfig(t, source, `apiVersion: v1
kind: Config
current-context: porto-dev
clusters:
  - name: porto-dev
    cluster:
      server: https://127.0.0.1:65432
contexts:
  - name: porto-dev
    context:
      cluster: porto-dev
      user: porto-dev
users:
  - name: porto-dev
    user:
      token: refreshed-token
`)

	if _, err := registry.Register(context.Background(), source, owner); err != nil {
		t.Fatalf("refresh managed bundle: %v", err)
	}
	document = readTestKubeconfig(t, target)
	if got := document["current-context"]; got != "dev-alias" {
		t.Fatalf("current context = %#v, want dev-alias", got)
	}
	alias := testMap(t, testNamedEntry(t, document, "contexts", "dev-alias")["context"])
	if alias["namespace"] != "external" || alias["cluster"] != "porto-dev" || alias["user"] != "porto-dev" {
		t.Fatalf("external alias changed: %#v", alias)
	}
	cluster := testMap(t, testNamedEntry(t, document, "clusters", "porto-dev")["cluster"])
	if cluster["server"] != "https://127.0.0.1:65432" {
		t.Fatalf("managed server = %#v", cluster["server"])
	}
	user := testMap(t, testNamedEntry(t, document, "users", "porto-dev")["user"])
	if user["token"] != "refreshed-token" {
		t.Fatalf("managed token = %#v", user["token"])
	}
}

func TestKubeconfigRegistryRejectsManagedContextOwnedByAnotherCluster(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config")
	source := filepath.Join(dir, "porto.yaml")
	writeTestKubeconfig(t, source, `apiVersion: v1
kind: Config
current-context: porto-k3s-dev
clusters:
  - name: porto-k3s-dev
    cluster:
      server: https://127.0.0.1:6443
contexts:
  - name: porto-k3s-dev
    context:
      cluster: porto-k3s-dev
      user: porto-k3s-dev
users:
  - name: porto-k3s-dev
    user:
      token: porto-token
`)
	registry := NewKubeconfigRegistry(target)
	if _, err := registry.Register(context.Background(), source, KubeconfigOwner{
		Cluster:  "dev",
		Provider: "k3s",
	}); err != nil {
		t.Fatal(err)
	}

	err := registry.CheckAvailable(context.Background(), "porto-k3s-dev", KubeconfigOwner{
		Cluster:  "k3s-dev",
		Provider: "kind",
	})
	if !errors.Is(err, errKubeconfigContextCollision) {
		t.Fatalf("managed owner collision error = %v", err)
	}
	err = registry.Remove(context.Background(), "porto-k3s-dev", source, KubeconfigOwner{
		Cluster:  "k3s-dev",
		Provider: "kind",
	})
	if !errors.Is(err, errKubeconfigContextCollision) {
		t.Fatalf("managed owner removal error = %v", err)
	}
	document := readTestKubeconfig(t, target)
	if testNamedEntry(t, document, "contexts", "porto-k3s-dev") == nil {
		t.Fatal("managed context was removed by another cluster owner")
	}
}

func TestKubeconfigRegistryRemovesManagedContextAndClearsCurrentContext(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config")
	source := filepath.Join(dir, "porto.yaml")
	writeTestKubeconfig(t, source, `apiVersion: v1
kind: Config
current-context: porto-dev
clusters:
  - name: porto-dev
    cluster:
      server: https://127.0.0.1:6443
contexts:
  - name: porto-dev
    context:
      cluster: porto-dev
      user: porto-dev
users:
  - name: porto-dev
    user:
      token: porto-token
`)
	registry := NewKubeconfigRegistry(target)
	if _, err := registry.Register(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	document := readTestKubeconfig(t, target)
	document["current-context"] = "porto-dev"
	contents, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := registry.Remove(context.Background(), "porto-dev", source); err != nil {
		t.Fatalf("remove managed context: %v", err)
	}
	document = readTestKubeconfig(t, target)
	if got := document["current-context"]; got != "" {
		t.Fatalf("current context = %#v, want empty", got)
	}
	for _, field := range []string{"contexts", "clusters", "users"} {
		if entry := testNamedEntry(t, document, field, "porto-dev"); entry != nil {
			t.Fatalf("%s entry was not removed: %#v", field, entry)
		}
	}
}

func TestKubeconfigRegistryRemovalPreservesExternalAlias(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config")
	source := filepath.Join(dir, "porto.yaml")
	writeTestKubeconfig(t, source, `apiVersion: v1
kind: Config
current-context: porto-dev
clusters:
  - name: porto-dev
    cluster:
      server: https://127.0.0.1:54321
contexts:
  - name: porto-dev
    context:
      cluster: porto-dev
      user: porto-dev
users:
  - name: porto-dev
    user:
      token: porto-token
`)
	registry := NewKubeconfigRegistry(target)
	owner := KubeconfigOwner{Cluster: "dev", Provider: "kind"}
	if _, err := registry.Register(context.Background(), source, owner); err != nil {
		t.Fatal(err)
	}
	document := readTestKubeconfig(t, target)
	document["current-context"] = "dev-alias"
	document["contexts"] = append(document["contexts"].([]any), map[string]any{
		"name": "dev-alias",
		"context": map[string]any{
			"cluster": "porto-dev",
			"user":    "porto-dev",
		},
	})
	contents, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := registry.Remove(context.Background(), "porto-dev", source, owner); err != nil {
		t.Fatalf("remove managed context: %v", err)
	}
	document = readTestKubeconfig(t, target)
	if got := document["current-context"]; got != "dev-alias" {
		t.Fatalf("current context = %#v, want dev-alias", got)
	}
	if testNamedEntry(t, document, "contexts", "porto-dev") != nil {
		t.Fatal("Porto context was not removed")
	}
	if testNamedEntry(t, document, "contexts", "dev-alias") == nil {
		t.Fatal("external alias was removed")
	}
	if testNamedEntry(t, document, "clusters", "porto-dev") == nil {
		t.Fatal("externally referenced cluster entry was removed")
	}
	if testNamedEntry(t, document, "users", "porto-dev") == nil {
		t.Fatal("externally referenced user entry was removed")
	}
}

func TestKubeconfigRegistryRenamesManagedContextAtomically(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config")
	oldSource := filepath.Join(dir, "old.yaml")
	newSource := filepath.Join(dir, "new.yaml")
	writeTestKubeconfig(t, oldSource, `apiVersion: v1
kind: Config
current-context: porto-dev
clusters:
  - name: porto-dev
    cluster:
      server: https://127.0.0.1:54321
contexts:
  - name: porto-dev
    context:
      cluster: porto-dev
      user: porto-dev
users:
  - name: porto-dev
    user:
      token: old-token
`)
	writeTestKubeconfig(t, newSource, `apiVersion: v1
kind: Config
current-context: porto-prod
clusters:
  - name: porto-prod
    cluster:
      server: https://127.0.0.1:54321
contexts:
  - name: porto-prod
    context:
      cluster: porto-prod
      user: porto-prod
users:
  - name: porto-prod
    user:
      token: old-token
`)
	registry := NewKubeconfigRegistry(target)
	if _, err := registry.Register(context.Background(), oldSource); err != nil {
		t.Fatal(err)
	}
	document := readTestKubeconfig(t, target)
	document["current-context"] = "porto-dev"
	contents, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	registration, err := registry.Rename(context.Background(), "porto-dev", newSource)
	if err != nil {
		t.Fatalf("rename managed context: %v", err)
	}
	if registration.Context != "porto-prod" {
		t.Fatalf("renamed context = %q, want porto-prod", registration.Context)
	}
	document = readTestKubeconfig(t, target)
	if got := document["current-context"]; got != "porto-prod" {
		t.Fatalf("current context = %#v, want porto-prod", got)
	}
	for _, field := range []string{"contexts", "clusters", "users"} {
		if entry := testNamedEntry(t, document, field, "porto-dev"); entry != nil {
			t.Fatalf("old %s entry was not removed: %#v", field, entry)
		}
		if entry := testNamedEntry(t, document, field, "porto-prod"); entry == nil {
			t.Fatalf("new %s entry was not installed", field)
		}
	}
}

func TestKubeconfigRegistryCreatesOneTimeBackup(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config")
	source := filepath.Join(dir, "porto.yaml")
	original := `apiVersion: v1
kind: Config
current-context: existing
clusters: []
contexts: []
users: []
`
	writeTestKubeconfig(t, target, original)
	writeTestKubeconfig(t, source, `apiVersion: v1
kind: Config
current-context: porto-dev
clusters:
  - name: porto-dev
    cluster:
      server: https://127.0.0.1:6443
contexts:
  - name: porto-dev
    context:
      cluster: porto-dev
      user: porto-dev
users:
  - name: porto-dev
    user:
      token: first-token
`)
	registry := NewKubeconfigRegistry(target)

	registration, err := registry.Register(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	backup := target + ".porto-backup"
	if registration.Backup != backup {
		t.Fatalf("backup path = %q, want %q", registration.Backup, backup)
	}
	backupContents, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if string(backupContents) != original {
		t.Fatalf("backup contents changed:\n%s", backupContents)
	}

	writeTestKubeconfig(t, source, `apiVersion: v1
kind: Config
current-context: porto-dev
clusters:
  - name: porto-dev
    cluster:
      server: https://127.0.0.1:6443
contexts:
  - name: porto-dev
    context:
      cluster: porto-dev
      user: porto-dev
users:
  - name: porto-dev
    user:
      token: refreshed-token
`)
	if _, err := registry.Register(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	backupContents, err = os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if string(backupContents) != original {
		t.Fatalf("one-time backup was replaced:\n%s", backupContents)
	}
}

func TestKubeconfigRegistryLockHonorsContextCancellation(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "config.lock")
	first, err := acquireKubeconfigFileLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}
	defer first.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := acquireKubeconfigFileLock(ctx, lockPath); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock error = %v, want context deadline exceeded", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("release first lock: %v", err)
	}
	second, err := acquireKubeconfigFileLock(context.Background(), lockPath)
	if err != nil {
		t.Fatalf("acquire released lock: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("release second lock: %v", err)
	}
}

func TestKubeconfigRegistryReleasesSharedLockFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config")
	source := filepath.Join(dir, "porto.yaml")
	writeTestKubeconfig(t, source, `apiVersion: v1
kind: Config
current-context: porto-dev
clusters:
  - name: porto-dev
    cluster:
      server: https://127.0.0.1:6443
contexts:
  - name: porto-dev
    context:
      cluster: porto-dev
      user: porto-dev
users:
  - name: porto-dev
    user:
      token: porto-token
`)

	if _, err := NewKubeconfigRegistry(target).Register(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	lockPath := target + ".lock"
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("kubeconfig lock remains after registration: %v", err)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("external kubeconfig writer cannot acquire lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
}

func TestKubeconfigRegistryReleasesLockAfterPanic(t *testing.T) {
	target := filepath.Join(t.TempDir(), "config")
	registry := NewKubeconfigRegistry(target)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("lock operation did not panic")
			}
		}()
		_ = registry.withLock(context.Background(), func(string) error {
			panic("test panic")
		})
	}()

	if _, err := os.Stat(target + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("kubeconfig lock remains after panic: %v", err)
	}
}

func TestKubeconfigRegistryRecoversDeadPortoLock(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config")
	source := filepath.Join(dir, "porto.yaml")
	writeTestKubeconfig(t, source, `apiVersion: v1
kind: Config
current-context: porto-dev
clusters:
  - name: porto-dev
    cluster:
      server: https://127.0.0.1:54321
contexts:
  - name: porto-dev
    context:
      cluster: porto-dev
      user: porto-dev
users:
  - name: porto-dev
    user:
      token: porto-token
`)
	lockPath := target + ".lock"
	owner, err := json.Marshal(kubeconfigLockOwner{
		PID:       999999999,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, owner, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	if _, err := NewKubeconfigRegistry(target).Register(ctx, source); err != nil {
		t.Fatalf("recover dead Porto lock: %v", err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered lock remains: %v", err)
	}
}

func TestKubeconfigRegistryRecoversOldExternalLock(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config")
	source := filepath.Join(dir, "porto.yaml")
	writeTestKubeconfig(t, source, `apiVersion: v1
kind: Config
current-context: porto-dev
clusters:
  - name: porto-dev
    cluster:
      server: https://127.0.0.1:54321
contexts:
  - name: porto-dev
    context:
      cluster: porto-dev
      user: porto-dev
users:
  - name: porto-dev
    user:
      token: porto-token
`)
	lockPath := target + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	staleTime := time.Now().Add(-kubeconfigStaleLockAge - time.Second)
	if err := os.Chtimes(lockPath, staleTime, staleTime); err != nil {
		t.Fatal(err)
	}

	if _, err := NewKubeconfigRegistry(target).Register(context.Background(), source); err != nil {
		t.Fatalf("recover old external lock: %v", err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered external lock remains: %v", err)
	}
}

func TestKubeconfigRegistryWaitsForFreshKubectlLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "config.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = acquireKubeconfigFileLock(ctx, lockPath)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fresh kubectl lock error = %v, want deadline exceeded", err)
	}
	if _, err := os.Lstat(lockPath); err != nil {
		t.Fatalf("fresh kubectl lock was removed: %v", err)
	}
}

func TestKubeconfigLockClosePreservesNewerReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not allow replacing an open lock file")
	}
	lockPath := filepath.Join(t.TempDir(), "config.lock")
	lock, err := acquireKubeconfigFileLock(context.Background(), lockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = lock.Close()
	})
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	replacement := []byte("new lock owner")
	if err := os.WriteFile(lockPath, replacement, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := lock.Close(); err != nil {
		t.Fatalf("close old lock: %v", err)
	}
	contents, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("newer lock was removed: %v", err)
	}
	if !bytes.Equal(contents, replacement) {
		t.Fatalf("newer lock changed: %q", contents)
	}
}

func TestKubeconfigRegistryPreservesSymlinkedTarget(t *testing.T) {
	dir := t.TempDir()
	actual := filepath.Join(dir, "shared", "config")
	target := filepath.Join(dir, ".kube", "config")
	source := filepath.Join(dir, "porto.yaml")
	writeTestKubeconfig(t, actual, `apiVersion: v1
kind: Config
current-context: ""
clusters: []
contexts: []
users: []
`)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(actual, target); err != nil {
		t.Fatal(err)
	}
	writeTestKubeconfig(t, source, `apiVersion: v1
kind: Config
current-context: porto-dev
clusters:
  - name: porto-dev
    cluster:
      server: https://127.0.0.1:6443
contexts:
  - name: porto-dev
    context:
      cluster: porto-dev
      user: porto-dev
users:
  - name: porto-dev
    user:
      token: porto-token
`)

	if _, err := NewKubeconfigRegistry(target).Register(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("global kubeconfig symlink was replaced")
	}
	if testNamedEntry(t, readTestKubeconfig(t, actual), "contexts", "porto-dev") == nil {
		t.Fatal("symlink target was not updated")
	}
}

func TestClusterCreationRegistersGlobalKubeconfig(t *testing.T) {
	runner := newFakeRunner()
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), ".kube", "config")
	provisioner := NewClusterProvisioner(
		vm.New(runner),
		runner,
		root,
		WithKubeconfigRegistry(NewKubeconfigRegistry(target)),
	)

	cluster, err := provisioner.Create(context.Background(), ClusterRequest{
		Name:     "dev",
		Provider: "kind",
	})
	if err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	document := readTestKubeconfig(t, target)
	if testNamedEntry(t, document, "contexts", cluster.Context) == nil {
		t.Fatalf("created context %q was not registered", cluster.Context)
	}
}

func TestClusterStartRefreshesGlobalKubeconfig(t *testing.T) {
	runner := newFakeRunner()
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		joined := strings.Join(command.Args, " ")
		switch {
		case command.Name == "kind" && joined == "get nodes --name porto-dev":
			return []byte("porto-dev-control-plane\n"), nil
		case command.Name == "docker" && strings.HasPrefix(joined, "start "):
			return nil, nil
		default:
			return newFakeRunner().Run(context.Background(), command)
		}
	}
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), ".kube", "config")
	registry := NewKubeconfigRegistry(target)
	provisioner := NewClusterProvisioner(
		vm.New(runner),
		runner,
		root,
		WithKubeconfigRegistry(registry),
	)
	if err := provisioner.writeClusterMetadata(ClusterRequest{Name: "dev", Provider: "kind"}); err != nil {
		t.Fatal(err)
	}
	source := provisioner.clusterKubeconfigPath("dev")
	writeTestKubeconfig(t, source, `apiVersion: v1
kind: Config
current-context: porto-dev
clusters:
  - name: porto-dev
    cluster:
      server: https://127.0.0.1:54321
contexts:
  - name: porto-dev
    context:
      cluster: porto-dev
      user: porto-dev
users:
  - name: porto-dev
    user:
      token: old-token
`)
	if _, err := registry.Register(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	writeTestKubeconfig(t, source, `apiVersion: v1
kind: Config
current-context: porto-dev
clusters:
  - name: porto-dev
    cluster:
      server: https://127.0.0.1:54322
contexts:
  - name: porto-dev
    context:
      cluster: porto-dev
      user: porto-dev
users:
  - name: porto-dev
    user:
      token: refreshed-token
`)

	if _, err := provisioner.Start(context.Background(), "dev"); err != nil {
		t.Fatalf("start cluster: %v", err)
	}
	document := readTestKubeconfig(t, target)
	cluster := testMap(t, testNamedEntry(t, document, "clusters", "porto-dev")["cluster"])
	if cluster["server"] != "https://127.0.0.1:54322" {
		t.Fatalf("registered server = %#v", cluster["server"])
	}
	user := testMap(t, testNamedEntry(t, document, "users", "porto-dev")["user"])
	if user["token"] != "refreshed-token" {
		t.Fatalf("registered token = %#v", user["token"])
	}
}

func TestKindRecreationRefreshesGlobalBundleAndPreservesExternalAlias(t *testing.T) {
	baseRunner := newFakeRunner()
	runner := newFakeRunner()
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		joined := strings.Join(command.Args, " ")
		switch {
		case command.Name == "kind" && joined == "get nodes --name porto-dev":
			return []byte("porto-dev-worker\n"), nil
		case command.Name == "kubectl" && strings.Contains(joined, "config view --raw -o json"):
			return []byte(`{
  "apiVersion": "v1",
  "kind": "Config",
  "current-context": "kind-porto-dev",
  "clusters": [{"name": "kind-porto-dev", "cluster": {"server": "https://127.0.0.1:65432"}}],
  "contexts": [{"name": "kind-porto-dev", "context": {"cluster": "kind-porto-dev", "user": "kind-porto-dev"}}],
  "users": [{"name": "kind-porto-dev", "user": {"client-certificate-data": "bmV3LWNlcnQ=", "client-key-data": "bmV3LWtleQ=="}}]
}`), nil
		case command.Name == "kubectl" && strings.Contains(joined, "config view --minify"):
			return []byte("https://127.0.0.1:65432"), nil
		default:
			return baseRunner.Run(context.Background(), command)
		}
	}
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), ".kube", "config")
	registry := NewKubeconfigRegistry(target)
	provisioner := NewClusterProvisioner(
		vm.New(runner),
		runner,
		root,
		WithKubeconfigRegistry(registry),
	)
	request := ClusterRequest{
		Name:       "dev",
		Provider:   "kind",
		NodeGroups: []NodeGroupSpec{{Name: "workers", Count: 1}},
	}
	if err := provisioner.writeClusterMetadata(request); err != nil {
		t.Fatal(err)
	}
	source := provisioner.clusterKubeconfigPath("dev")
	writeTestKubeconfig(t, source, `{
  "apiVersion": "v1",
  "kind": "Config",
  "current-context": "porto-dev",
  "clusters": [{"name": "porto-dev", "cluster": {"server": "https://127.0.0.1:54321"}}],
  "contexts": [{"name": "porto-dev", "context": {"cluster": "porto-dev", "user": "porto-dev"}}],
  "users": [{"name": "porto-dev", "user": {"client-certificate-data": "b2xkLWNlcnQ=", "client-key-data": "b2xkLWtleQ=="}}]
}`)
	owner := clusterKubeconfigOwner(request)
	if _, err := registry.Register(context.Background(), source, owner); err != nil {
		t.Fatal(err)
	}
	document := readTestKubeconfig(t, target)
	document["current-context"] = "dev-alias"
	document["contexts"] = append(document["contexts"].([]any), map[string]any{
		"name": "dev-alias",
		"context": map[string]any{
			"cluster": "porto-dev",
			"user":    "porto-dev",
		},
	})
	contents, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	recreated, err := provisioner.Start(context.Background(), "dev")
	if err != nil {
		t.Fatalf("recreate kind cluster: %v", err)
	}
	if !recreated {
		t.Fatal("missing kind control plane was not recreated")
	}
	document = readTestKubeconfig(t, target)
	if got := document["current-context"]; got != "dev-alias" {
		t.Fatalf("current context = %#v, want dev-alias", got)
	}
	if testNamedEntry(t, document, "contexts", "dev-alias") == nil {
		t.Fatal("external alias was removed")
	}
	cluster := testMap(t, testNamedEntry(t, document, "clusters", "porto-dev")["cluster"])
	if cluster["server"] != "https://127.0.0.1:65432" {
		t.Fatalf("recreated kind server = %#v", cluster["server"])
	}
	user := testMap(t, testNamedEntry(t, document, "users", "porto-dev")["user"])
	if user["client-certificate-data"] != "bmV3LWNlcnQ=" ||
		user["client-key-data"] != "bmV3LWtleQ==" {
		t.Fatalf("recreated kind credentials = %#v", user)
	}
}

func TestStoppingKindClusterKeepsRegisteredContext(t *testing.T) {
	runner := newFakeRunner()
	target := filepath.Join(t.TempDir(), ".kube", "config")
	provisioner := NewClusterProvisioner(
		vm.New(runner),
		runner,
		t.TempDir(),
		WithKubeconfigRegistry(NewKubeconfigRegistry(target)),
	)
	request := ClusterRequest{Name: "dev", Provider: "kind"}
	if err := provisioner.writeClusterMetadata(request); err != nil {
		t.Fatal(err)
	}
	source := provisioner.clusterKubeconfigPath("dev")
	writeTestKubeconfig(t, source, `{
  "apiVersion": "v1",
  "kind": "Config",
  "current-context": "porto-dev",
  "clusters": [{"name": "porto-dev", "cluster": {"server": "https://127.0.0.1:54321"}}],
  "contexts": [{"name": "porto-dev", "context": {"cluster": "porto-dev", "user": "porto-dev"}}],
  "users": [{"name": "porto-dev", "user": {"client-certificate-data": "Y2VydA==", "client-key-data": "a2V5"}}]
}`)
	if _, err := provisioner.registerKubeconfig(context.Background(), "dev"); err != nil {
		t.Fatal(err)
	}

	if err := provisioner.SetRunning(context.Background(), "dev", false); err != nil {
		t.Fatalf("stop kind cluster: %v", err)
	}
	document := readTestKubeconfig(t, target)
	if testNamedEntry(t, document, "contexts", "porto-dev") == nil {
		t.Fatal("stopped kind cluster was removed from the global kubeconfig")
	}
	cluster := testMap(t, testNamedEntry(t, document, "clusters", "porto-dev")["cluster"])
	if cluster["server"] != "https://127.0.0.1:54321" {
		t.Fatalf("stopped kind server changed: %#v", cluster["server"])
	}
}

func TestVMClusterStartRefetchesGlobalKubeconfigCredentials(t *testing.T) {
	baseRunner := newFakeRunner()
	runner := newFakeRunner()
	refetched := false
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		joined := strings.Join(command.Args, " ")
		switch {
		case command.Name == "limactl" && strings.Contains(joined, "/etc/rancher/k3s/k3s.yaml"):
			refetched = true
			return []byte(`apiVersion: v1
kind: Config
current-context: default
clusters:
  - name: default
    cluster:
      server: https://127.0.0.1:6443
contexts:
  - name: default
    context:
      cluster: default
      user: default
users:
  - name: default
    user:
      token: refreshed-token
`), nil
		case command.Name == "kubectl" && strings.Contains(joined, "config view --raw -o json"):
			kubeconfigPath := ""
			for index, arg := range command.Args {
				if arg == "--kubeconfig" && index+1 < len(command.Args) {
					kubeconfigPath = command.Args[index+1]
				}
			}
			document := readTestKubeconfig(t, kubeconfigPath)
			return json.Marshal(document)
		default:
			return baseRunner.Run(context.Background(), command)
		}
	}
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), ".kube", "config")
	registry := NewKubeconfigRegistry(target)
	provisioner := NewClusterProvisioner(
		vm.New(runner),
		runner,
		root,
		WithKubeconfigRegistry(registry),
	)
	request := ClusterRequest{Name: "dev", Provider: "k3s", APIPort: 54321}
	if err := provisioner.writeClusterMetadata(request); err != nil {
		t.Fatal(err)
	}
	source := provisioner.clusterKubeconfigPath("dev")
	writeTestKubeconfig(t, source, `{
  "apiVersion": "v1",
  "kind": "Config",
  "current-context": "porto-k3s-dev",
  "clusters": [{"name": "porto-k3s-dev", "cluster": {"server": "https://127.0.0.1:54321"}}],
  "contexts": [{"name": "porto-k3s-dev", "context": {"cluster": "porto-k3s-dev", "user": "porto-k3s-dev"}}],
  "users": [{"name": "porto-k3s-dev", "user": {"token": "old-token"}}]
}`)
	if _, err := registry.Register(context.Background(), source); err != nil {
		t.Fatal(err)
	}

	if _, err := provisioner.Start(context.Background(), "dev"); err != nil {
		t.Fatalf("start k3s cluster: %v", err)
	}
	if !refetched {
		t.Fatal("k3s admin kubeconfig was not re-fetched")
	}
	document := readTestKubeconfig(t, target)
	user := testMap(t, testNamedEntry(t, document, "users", "porto-k3s-dev")["user"])
	if user["token"] != "refreshed-token" {
		t.Fatalf("registered token = %#v", user["token"])
	}
	cluster := testMap(t, testNamedEntry(t, document, "clusters", "porto-k3s-dev")["cluster"])
	if cluster["server"] != "https://127.0.0.1:54321" {
		t.Fatalf("registered server = %#v", cluster["server"])
	}
}

func TestK0sRegistrationUsesHostEndpointAndAdminCredentials(t *testing.T) {
	baseRunner := newFakeRunner()
	runner := newFakeRunner()
	fetchedAdminConfig := false
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		joined := strings.Join(command.Args, " ")
		switch {
		case command.Name == "limactl" && strings.Contains(joined, "k0s kubeconfig admin"):
			fetchedAdminConfig = true
			return []byte(`apiVersion: v1
kind: Config
current-context: default
clusters:
  - name: default
    cluster:
      server: https://192.168.105.2:6443
contexts:
  - name: default
    context:
      cluster: default
      user: default
users:
  - name: default
    user:
      token: k0s-admin-token
`), nil
		case command.Name == "kubectl" && strings.Contains(joined, "config view --raw -o json"):
			path := ""
			for index, arg := range command.Args {
				if arg == "--kubeconfig" && index+1 < len(command.Args) {
					path = command.Args[index+1]
				}
			}
			return json.Marshal(readTestKubeconfig(t, path))
		default:
			return baseRunner.Run(context.Background(), command)
		}
	}
	target := filepath.Join(t.TempDir(), ".kube", "config")
	provisioner := NewClusterProvisioner(
		vm.New(runner),
		runner,
		t.TempDir(),
		WithKubeconfigRegistry(NewKubeconfigRegistry(target)),
	)

	cluster, err := provisioner.Create(context.Background(), ClusterRequest{
		Name:         "dev",
		Provider:     "k0s",
		ControlPlane: MachineSpec{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20},
	})
	if err != nil {
		t.Fatalf("create k0s cluster: %v", err)
	}
	if !fetchedAdminConfig {
		t.Fatal("k0s admin kubeconfig was not fetched")
	}
	document := readTestKubeconfig(t, target)
	contextEntry := testNamedEntry(t, document, "contexts", "porto-dev")
	if contextEntry == nil {
		t.Fatal("k0s context was not registered")
	}
	clusterEntry := testMap(t, testNamedEntry(t, document, "clusters", "porto-dev")["cluster"])
	if clusterEntry["server"] != cluster.Server {
		t.Fatalf("registered k0s server = %#v, want %q", clusterEntry["server"], cluster.Server)
	}
	if clusterEntry["server"] == "https://192.168.105.2:6443" {
		t.Fatal("k0s registration retained the VM-local API endpoint")
	}
	user := testMap(t, testNamedEntry(t, document, "users", "porto-dev")["user"])
	if user["token"] != "k0s-admin-token" {
		t.Fatalf("registered k0s credentials = %#v", user)
	}
}

func TestRefreshVMKubeconfigRejectsMissingAPIPort(t *testing.T) {
	runner := newFakeRunner()
	provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())

	err := provisioner.refreshVMKubeconfig(
		context.Background(),
		ClusterRequest{Name: "dev", Provider: "k3s"},
		"porto-dev-server-1",
	)
	if err == nil || !strings.Contains(err.Error(), "API port") {
		t.Fatalf("refresh error = %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.commands) != 0 {
		t.Fatalf("refresh invoked commands with a missing API port: %+v", runner.commands)
	}
}

func TestRefreshVMKubeconfigRetriesUntilControllerIsReady(t *testing.T) {
	runner := newFakeRunner()
	attempts := 0
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		joined := strings.Join(command.Args, " ")
		switch {
		case command.Name == "limactl" && strings.Contains(joined, "/etc/rancher/k3s/k3s.yaml"):
			attempts++
			if attempts == 1 {
				return []byte("controller not ready"), errors.New("exit status 1")
			}
			return []byte(`apiVersion: v1
kind: Config
current-context: default
clusters:
  - name: default
    cluster:
      server: https://127.0.0.1:6443
contexts:
  - name: default
    context:
      cluster: default
      user: default
users:
  - name: default
    user:
      token: refreshed-token
`), nil
		case command.Name == "kubectl" && strings.Contains(joined, "config view --raw -o json"):
			path := ""
			for index, arg := range command.Args {
				if arg == "--kubeconfig" && index+1 < len(command.Args) {
					path = command.Args[index+1]
				}
			}
			return json.Marshal(readTestKubeconfig(t, path))
		default:
			return nil, nil
		}
	}
	provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())

	if err := provisioner.refreshVMKubeconfig(
		context.Background(),
		ClusterRequest{Name: "dev", Provider: "k3s", APIPort: 54321},
		"porto-dev-server-1",
	); err != nil {
		t.Fatalf("refresh kubeconfig: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("credential fetch attempts = %d, want 2", attempts)
	}
}

func TestRefreshVMKubeconfigKeepsPreviousFileWhenNormalizationFails(t *testing.T) {
	runner := newFakeRunner()
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		joined := strings.Join(command.Args, " ")
		switch {
		case command.Name == "limactl" && strings.Contains(joined, "/etc/rancher/k3s/k3s.yaml"):
			return []byte(`apiVersion: v1
kind: Config
current-context: default
clusters:
  - name: default
    cluster:
      server: https://127.0.0.1:6443
contexts:
  - name: default
    context:
      cluster: default
      user: default
users:
  - name: default
    user:
      token: refreshed-token
`), nil
		case command.Name == "kubectl" && strings.Contains(joined, "config view --raw -o json"):
			return []byte("normalization failed"), errors.New("exit status 1")
		default:
			return nil, nil
		}
	}
	provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
	path := provisioner.clusterKubeconfigPath("dev")
	original := `{
  "apiVersion": "v1",
  "kind": "Config",
  "current-context": "porto-k3s-dev",
  "clusters": [{"name": "porto-k3s-dev", "cluster": {"server": "https://127.0.0.1:54321"}}],
  "contexts": [{"name": "porto-k3s-dev", "context": {"cluster": "porto-k3s-dev", "user": "porto-k3s-dev"}}],
  "users": [{"name": "porto-k3s-dev", "user": {"token": "old-token"}}]
}`
	writeTestKubeconfig(t, path, original)

	err := provisioner.refreshVMKubeconfig(
		context.Background(),
		ClusterRequest{Name: "dev", Provider: "k3s", APIPort: 54321},
		"porto-dev-server-1",
	)
	if err == nil {
		t.Fatal("normalization failure returned success")
	}
	contents, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(contents) != original {
		t.Fatalf("previous kubeconfig was replaced after normalization failure:\n%s", contents)
	}
}

func TestClusterRenameUpdatesGlobalKubeconfig(t *testing.T) {
	runner := newFakeRunner()
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), ".kube", "config")
	registry := NewKubeconfigRegistry(target)
	provisioner := NewClusterProvisioner(
		vm.New(runner),
		runner,
		root,
		WithKubeconfigRegistry(registry),
	)
	if err := provisioner.writeClusterMetadata(ClusterRequest{Name: "dev", Provider: "kind"}); err != nil {
		t.Fatal(err)
	}
	source := provisioner.clusterKubeconfigPath("dev")
	writeTestKubeconfig(t, source, `{
  "apiVersion": "v1",
  "kind": "Config",
  "current-context": "porto-dev",
  "clusters": [{"name": "porto-dev", "cluster": {"server": "https://127.0.0.1:54321"}}],
  "contexts": [{"name": "porto-dev", "context": {"cluster": "porto-dev", "user": "porto-dev"}}],
  "users": [{"name": "porto-dev", "user": {"token": "porto-token"}}]
}`)
	if _, err := registry.Register(context.Background(), source); err != nil {
		t.Fatal(err)
	}

	if err := provisioner.Rename(context.Background(), "dev", "prod"); err != nil {
		t.Fatalf("rename cluster: %v", err)
	}
	document := readTestKubeconfig(t, target)
	if testNamedEntry(t, document, "contexts", "porto-dev") != nil {
		t.Fatal("old global context remains after rename")
	}
	newContext := testNamedEntry(t, document, "contexts", "porto-prod")
	if newContext == nil || !testHasManagedExtension(t, testMap(t, newContext["context"])) {
		t.Fatalf("new managed context missing: %#v", newContext)
	}
}

func TestClusterRenameRollsBackWhenGlobalContextCollides(t *testing.T) {
	runner := newFakeRunner()
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), ".kube", "config")
	registry := NewKubeconfigRegistry(target)
	provisioner := NewClusterProvisioner(
		vm.New(runner),
		runner,
		root,
		WithKubeconfigRegistry(registry),
	)
	if err := provisioner.writeClusterMetadata(ClusterRequest{Name: "dev", Provider: "kind"}); err != nil {
		t.Fatal(err)
	}
	source := provisioner.clusterKubeconfigPath("dev")
	writeTestKubeconfig(t, source, `{
  "apiVersion": "v1",
  "kind": "Config",
  "current-context": "porto-dev",
  "clusters": [{"name": "porto-dev", "cluster": {"server": "https://127.0.0.1:54321"}}],
  "contexts": [{"name": "porto-dev", "context": {"cluster": "porto-dev", "user": "porto-dev"}}],
  "users": [{"name": "porto-dev", "user": {"token": "porto-token"}}]
}`)
	if _, err := registry.Register(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	targetDocument := readTestKubeconfig(t, target)
	targetDocument["clusters"] = append(targetDocument["clusters"].([]any), map[string]any{
		"name": "porto-prod",
		"cluster": map[string]any{
			"server": "https://unrelated.example",
		},
	})
	targetDocument["contexts"] = append(targetDocument["contexts"].([]any), map[string]any{
		"name": "porto-prod",
		"context": map[string]any{
			"cluster": "porto-prod",
			"user":    "porto-prod",
		},
	})
	targetDocument["users"] = append(targetDocument["users"].([]any), map[string]any{
		"name": "porto-prod",
		"user": map[string]any{
			"token": "unrelated-token",
		},
	})
	targetContents, err := yaml.Marshal(targetDocument)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, targetContents, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := provisioner.Rename(context.Background(), "dev", "prod"); err == nil {
		t.Fatal("rename collision returned success")
	}
	if _, err := os.Stat(provisioner.clusterKubeconfigPath("dev")); err != nil {
		t.Fatalf("old private kubeconfig was not preserved: %v", err)
	}
	if _, err := os.Stat(provisioner.clusterMetadataPath("dev")); err != nil {
		t.Fatalf("old cluster metadata was not preserved: %v", err)
	}
	if _, err := os.Stat(provisioner.clusterKubeconfigPath("prod")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new private kubeconfig remains after rollback: %v", err)
	}
	document := readTestKubeconfig(t, target)
	if testNamedEntry(t, document, "contexts", "porto-dev") == nil {
		t.Fatal("old managed context was removed after failed rename")
	}
}

func TestClusterDeletionRemovesGlobalKubeconfig(t *testing.T) {
	runner := newFakeRunner()
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), ".kube", "config")
	registry := NewKubeconfigRegistry(target)
	provisioner := NewClusterProvisioner(
		vm.New(runner),
		runner,
		root,
		WithKubeconfigRegistry(registry),
	)
	if err := provisioner.writeClusterMetadata(ClusterRequest{Name: "dev", Provider: "kind"}); err != nil {
		t.Fatal(err)
	}
	source := provisioner.clusterKubeconfigPath("dev")
	writeTestKubeconfig(t, source, `{
  "apiVersion": "v1",
  "kind": "Config",
  "current-context": "porto-dev",
  "clusters": [{"name": "porto-dev", "cluster": {"server": "https://127.0.0.1:54321"}}],
  "contexts": [{"name": "porto-dev", "context": {"cluster": "porto-dev", "user": "porto-dev"}}],
  "users": [{"name": "porto-dev", "user": {"token": "porto-token"}}]
}`)
	if _, err := registry.Register(context.Background(), source); err != nil {
		t.Fatal(err)
	}

	if err := provisioner.Delete(context.Background(), "dev"); err != nil {
		t.Fatalf("delete cluster: %v", err)
	}
	document := readTestKubeconfig(t, target)
	for _, field := range []string{"contexts", "clusters", "users"} {
		if entry := testNamedEntry(t, document, field, "porto-dev"); entry != nil {
			t.Fatalf("deleted cluster remains in global %s: %#v", field, entry)
		}
	}
}

func TestClusterDeletionRemovesPrivateStateWhenGlobalCleanupFails(t *testing.T) {
	runner := newFakeRunner()
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), ".kube", "config")
	writeTestKubeconfig(t, target, "{")
	provisioner := NewClusterProvisioner(
		vm.New(runner),
		runner,
		root,
		WithKubeconfigRegistry(NewKubeconfigRegistry(target)),
	)
	if err := provisioner.writeClusterMetadata(ClusterRequest{Name: "dev", Provider: "kind"}); err != nil {
		t.Fatal(err)
	}
	writeTestKubeconfig(t, provisioner.clusterKubeconfigPath("dev"), `{
  "apiVersion": "v1",
  "kind": "Config",
  "current-context": "porto-dev",
  "clusters": [{"name": "porto-dev", "cluster": {"server": "https://127.0.0.1:54321"}}],
  "contexts": [{"name": "porto-dev", "context": {"cluster": "porto-dev", "user": "porto-dev"}}],
  "users": [{"name": "porto-dev", "user": {"token": "porto-token"}}]
}`)

	if err := provisioner.Delete(context.Background(), "dev"); err == nil {
		t.Fatal("global cleanup failure returned success")
	}
	for _, path := range []string{
		provisioner.clusterKubeconfigPath("dev"),
		provisioner.clusterMetadataPath("dev"),
	} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("private cluster state remains at %s: %v", path, err)
		}
	}
}

func TestClusterCreationRejectsGlobalCollisionBeforeProvisioning(t *testing.T) {
	runner := newFakeRunner()
	target := filepath.Join(t.TempDir(), ".kube", "config")
	writeTestKubeconfig(t, target, `apiVersion: v1
kind: Config
current-context: porto-dev
clusters:
  - name: porto-dev
    cluster:
      server: https://unrelated.example
contexts:
  - name: porto-dev
    context:
      cluster: porto-dev
      user: porto-dev
users:
  - name: porto-dev
    user:
      token: unrelated-token
`)
	provisioner := NewClusterProvisioner(
		vm.New(runner),
		runner,
		t.TempDir(),
		WithKubeconfigRegistry(NewKubeconfigRegistry(target)),
	)

	if _, err := provisioner.Create(context.Background(), ClusterRequest{Name: "dev", Provider: "kind"}); err == nil {
		t.Fatal("global context collision returned success")
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, command := range runner.commands {
		if command.Name == "kind" && len(command.Args) > 0 && command.Args[0] == "create" {
			t.Fatalf("cluster provisioning started before collision check: %+v", command)
		}
	}
}

func TestClusterCreationRejectsContextOwnedByDifferentProviderCluster(t *testing.T) {
	target := filepath.Join(t.TempDir(), ".kube", "config")
	source := filepath.Join(t.TempDir(), "existing.yaml")
	writeTestKubeconfig(t, source, `{
  "apiVersion": "v1",
  "kind": "Config",
  "current-context": "porto-k3s-dev",
  "clusters": [{"name": "porto-k3s-dev", "cluster": {"server": "https://127.0.0.1:50000"}}],
  "contexts": [{"name": "porto-k3s-dev", "context": {"cluster": "porto-k3s-dev", "user": "porto-k3s-dev"}}],
  "users": [{"name": "porto-k3s-dev", "user": {"token": "existing-token"}}]
}`)
	registry := NewKubeconfigRegistry(target)
	if _, err := registry.Register(context.Background(), source, KubeconfigOwner{
		Cluster:  "dev",
		Provider: "k3s",
	}); err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	provisioner := NewClusterProvisioner(
		vm.New(runner),
		runner,
		t.TempDir(),
		WithKubeconfigRegistry(registry),
	)

	if _, err := provisioner.Create(
		context.Background(),
		ClusterRequest{Name: "k3s-dev", Provider: "kind"},
	); !errors.Is(err, errKubeconfigContextCollision) {
		t.Fatalf("cross-provider context collision error = %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, command := range runner.commands {
		if command.Name == "kind" && len(command.Args) > 0 && command.Args[0] == "create" {
			t.Fatalf("cluster provisioning started despite owner collision: %+v", command)
		}
	}
}

func TestKindCreationCleansUpWhenLateGlobalCollisionAppears(t *testing.T) {
	target := filepath.Join(t.TempDir(), ".kube", "config")
	baseRunner := newFakeRunner()
	runner := newFakeRunner()
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if command.Name == "kind" && len(command.Args) > 0 && command.Args[0] == "create" {
			writeTestKubeconfig(t, target, `apiVersion: v1
kind: Config
current-context: porto-dev
clusters:
  - name: porto-dev
    cluster:
      server: https://unrelated.example
contexts:
  - name: porto-dev
    context:
      cluster: porto-dev
      user: porto-dev
users:
  - name: porto-dev
    user:
      token: unrelated-token
`)
		}
		return baseRunner.Run(context.Background(), command)
	}
	provisioner := NewClusterProvisioner(
		vm.New(runner),
		runner,
		t.TempDir(),
		WithKubeconfigRegistry(NewKubeconfigRegistry(target)),
	)

	if _, err := provisioner.Create(context.Background(), ClusterRequest{Name: "dev", Provider: "kind"}); err == nil {
		t.Fatal("late global context collision returned success")
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, command := range runner.commands {
		if command.Name == "kind" && len(command.Args) > 1 &&
			command.Args[0] == "delete" && command.Args[1] == "cluster" {
			return
		}
	}
	t.Fatal("kind cluster was not cleaned up after registration failure")
}

func TestKindCreationSurvivesTransientGlobalKubeconfigFailure(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "config")
	runner := newFakeRunner()
	provisioner := NewClusterProvisioner(
		vm.New(runner),
		runner,
		t.TempDir(),
		WithKubeconfigRegistry(NewKubeconfigRegistry(target)),
	)

	cluster, err := provisioner.Create(context.Background(), ClusterRequest{Name: "dev", Provider: "kind"})
	if err != nil {
		t.Fatalf("transient global kubeconfig failure aborted cluster creation: %v", err)
	}
	if cluster.Message == "" {
		t.Fatal("cluster creation did not report the registration warning")
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, command := range runner.commands {
		if command.Name == "kind" && len(command.Args) > 1 &&
			command.Args[0] == "delete" && command.Args[1] == "cluster" {
			t.Fatalf("healthy kind cluster was deleted after transient registration failure: %+v", command)
		}
	}
}

func TestClusterStartSurvivesTransientGlobalKubeconfigFailure(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	provisioner := NewClusterProvisioner(
		vm.New(runner),
		runner,
		t.TempDir(),
		WithKubeconfigRegistry(NewKubeconfigRegistry(filepath.Join(parent, "config"))),
	)
	if err := provisioner.writeClusterMetadata(ClusterRequest{Name: "dev", Provider: "kind"}); err != nil {
		t.Fatal(err)
	}
	writeTestKubeconfig(t, provisioner.clusterKubeconfigPath("dev"), `{
  "apiVersion": "v1",
  "kind": "Config",
  "current-context": "porto-dev",
  "clusters": [{"name": "porto-dev", "cluster": {"server": "https://127.0.0.1:54321"}}],
  "contexts": [{"name": "porto-dev", "context": {"cluster": "porto-dev", "user": "porto-dev"}}],
  "users": [{"name": "porto-dev", "user": {"token": "porto-token"}}]
}`)

	if _, err := provisioner.Start(context.Background(), "dev"); err != nil {
		t.Fatalf("transient global kubeconfig failure aborted cluster start: %v", err)
	}
}

func TestClusterKubeconfigReconciliationRegistersExistingAndPrunesStaleContexts(t *testing.T) {
	runner := newFakeRunner()
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), ".kube", "config")
	registry := NewKubeconfigRegistry(target)
	provisioner := NewClusterProvisioner(
		vm.New(runner),
		runner,
		root,
		WithKubeconfigRegistry(registry),
	)
	if err := provisioner.writeClusterMetadata(ClusterRequest{Name: "dev", Provider: "kind"}); err != nil {
		t.Fatal(err)
	}
	writeTestKubeconfig(t, provisioner.clusterKubeconfigPath("dev"), `{
  "apiVersion": "v1",
  "kind": "Config",
  "current-context": "porto-dev",
  "clusters": [{"name": "porto-dev", "cluster": {"server": "https://127.0.0.1:54321"}}],
  "contexts": [{"name": "porto-dev", "context": {"cluster": "porto-dev", "user": "porto-dev"}}],
  "users": [{"name": "porto-dev", "user": {"token": "porto-token"}}]
}`)
	staleSource := filepath.Join(t.TempDir(), "stale.yaml")
	writeTestKubeconfig(t, staleSource, `{
  "apiVersion": "v1",
  "kind": "Config",
  "current-context": "porto-old",
  "clusters": [{"name": "porto-old", "cluster": {"server": "https://127.0.0.1:50000"}}],
  "contexts": [{"name": "porto-old", "context": {"cluster": "porto-old", "user": "porto-old"}}],
  "users": [{"name": "porto-old", "user": {"token": "stale-token"}}]
}`)
	if _, err := registry.Register(context.Background(), staleSource); err != nil {
		t.Fatal(err)
	}
	targetDocument := readTestKubeconfig(t, target)
	targetDocument["clusters"] = append(targetDocument["clusters"].([]any), map[string]any{
		"name": "personal",
		"cluster": map[string]any{
			"server": "https://personal.example",
		},
	})
	targetDocument["contexts"] = append(targetDocument["contexts"].([]any), map[string]any{
		"name": "personal",
		"context": map[string]any{
			"cluster": "personal",
			"user":    "personal",
		},
	})
	targetDocument["users"] = append(targetDocument["users"].([]any), map[string]any{
		"name": "personal",
		"user": map[string]any{
			"token": "personal-token",
		},
	})
	targetContents, err := yaml.Marshal(targetDocument)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, targetContents, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := provisioner.ReconcileKubeconfigs(context.Background()); err != nil {
		t.Fatalf("reconcile kubeconfigs: %v", err)
	}
	document := readTestKubeconfig(t, target)
	if testNamedEntry(t, document, "contexts", "porto-dev") == nil {
		t.Fatal("existing Porto cluster was not registered")
	}
	if testNamedEntry(t, document, "contexts", "porto-old") != nil {
		t.Fatal("stale Porto context was not pruned")
	}
	if testNamedEntry(t, document, "contexts", "personal") == nil {
		t.Fatal("unmanaged context was pruned")
	}
}

func TestClusterKubeconfigReconciliationDoesNotPruneAfterMetadataError(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), ".kube", "config")
	registry := NewKubeconfigRegistry(target)
	staleSource := filepath.Join(t.TempDir(), "stale.yaml")
	writeTestKubeconfig(t, staleSource, `{
  "apiVersion": "v1",
  "kind": "Config",
  "current-context": "porto-old",
  "clusters": [{"name": "porto-old", "cluster": {"server": "https://127.0.0.1:50000"}}],
  "contexts": [{"name": "porto-old", "context": {"cluster": "porto-old", "user": "porto-old"}}],
  "users": [{"name": "porto-old", "user": {"token": "stale-token"}}]
}`)
	if _, err := registry.Register(context.Background(), staleSource); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	provisioner := NewClusterProvisioner(
		vm.New(newFakeRunner()),
		newFakeRunner(),
		root,
		WithKubeconfigRegistry(registry),
	)

	if err := provisioner.ReconcileKubeconfigs(context.Background()); err == nil {
		t.Fatal("invalid cluster metadata returned success")
	}
	document := readTestKubeconfig(t, target)
	if testNamedEntry(t, document, "contexts", "porto-old") == nil {
		t.Fatal("managed context was pruned after incomplete reconciliation")
	}
}

func TestClusterKubeconfigReconciliationMigratesLegacyK3sContext(t *testing.T) {
	runner := newFakeRunner()
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), ".kube", "config")
	provisioner := NewClusterProvisioner(
		vm.New(runner),
		runner,
		root,
		WithKubeconfigRegistry(NewKubeconfigRegistry(target)),
	)
	if err := provisioner.writeClusterMetadata(ClusterRequest{Name: "dev", Provider: "k3s"}); err != nil {
		t.Fatal(err)
	}
	writeTestKubeconfig(t, provisioner.clusterKubeconfigPath("dev"), `{
  "apiVersion": "v1",
  "kind": "Config",
  "current-context": "porto-dev",
  "clusters": [{"name": "porto-dev", "cluster": {"server": "https://127.0.0.1:54321"}}],
  "contexts": [{"name": "porto-dev", "context": {"cluster": "porto-dev", "user": "porto-dev"}}],
  "users": [{"name": "porto-dev", "user": {"token": "legacy-token"}}]
}`)

	if err := provisioner.ReconcileKubeconfigs(context.Background()); err != nil {
		t.Fatalf("reconcile legacy k3s context: %v", err)
	}
	document := readTestKubeconfig(t, target)
	if testNamedEntry(t, document, "contexts", "porto-k3s-dev") == nil {
		t.Fatal("migrated k3s context was not registered")
	}
	if testNamedEntry(t, document, "contexts", "porto-dev") != nil {
		t.Fatal("legacy k3s context was registered")
	}
}

func TestClusterKubeconfigReconciliationRejectsEmptyPrivateRootWithoutPruning(t *testing.T) {
	target := filepath.Join(t.TempDir(), ".kube", "config")
	registry := NewKubeconfigRegistry(target)
	source := filepath.Join(t.TempDir(), "cluster.yaml")
	writeTestKubeconfig(t, source, `{
  "apiVersion": "v1",
  "kind": "Config",
  "current-context": "porto-dev",
  "clusters": [{"name": "porto-dev", "cluster": {"server": "https://127.0.0.1:50000"}}],
  "contexts": [{"name": "porto-dev", "context": {"cluster": "porto-dev", "user": "porto-dev"}}],
  "users": [{"name": "porto-dev", "user": {"token": "porto-token"}}]
}`)
	if _, err := registry.Register(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	provisioner := NewClusterProvisioner(
		vm.New(newFakeRunner()),
		newFakeRunner(),
		"",
		WithKubeconfigRegistry(registry),
	)

	if err := provisioner.ReconcileKubeconfigs(context.Background()); err == nil {
		t.Fatal("empty private kubeconfig root returned success")
	}
	document := readTestKubeconfig(t, target)
	if testNamedEntry(t, document, "contexts", "porto-dev") == nil {
		t.Fatal("managed context was pruned for an invalid private root")
	}
}

func TestNormalizeKubeconfigFlattensReferencedCredentials(t *testing.T) {
	runner := newFakeRunner()
	provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
	path := filepath.Join(t.TempDir(), "config")

	if err := provisioner.normalizeKubeconfig(context.Background(), path, "porto-dev"); err != nil {
		t.Fatalf("normalize kubeconfig: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, command := range runner.commands {
		if command.Name == "kubectl" && strings.Contains(strings.Join(command.Args, " "), "config view") {
			if !slices.Contains(command.Args, "--flatten") {
				t.Fatalf("normalize command did not flatten credentials: %+v", command.Args)
			}
			return
		}
	}
	t.Fatal("normalize kubeconfig command was not executed")
}

func writeTestKubeconfig(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTestKubeconfig(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode kubeconfig: %v", err)
	}
	return document
}

func testNamedEntry(t *testing.T, document map[string]any, field, name string) map[string]any {
	t.Helper()
	entries, ok := document[field].([]any)
	if !ok {
		t.Fatalf("%s = %#v, want list", field, document[field])
	}
	for _, rawEntry := range entries {
		entry := testMap(t, rawEntry)
		if entry["name"] == name {
			return entry
		}
	}
	return nil
}

func testMap(t *testing.T, value any) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("value = %#v, want map", value)
	}
	return result
}

func testHasManagedExtension(t *testing.T, contextValue map[string]any) bool {
	t.Helper()
	extensions, ok := contextValue["extensions"].([]any)
	if !ok {
		return false
	}
	for _, rawExtension := range extensions {
		extension := testMap(t, rawExtension)
		if extension["name"] == managedKubeconfigExtension {
			return true
		}
	}
	return false
}
