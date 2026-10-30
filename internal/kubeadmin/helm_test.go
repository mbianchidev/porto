package kubeadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/localtools"
	"github.com/mbianchidev/porto/internal/runtimes"
	"k8s.io/client-go/tools/clientcmd"
)

const syntheticHelmManifest = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: synthetic-release-config\n  namespace: default\ndata:\n  setting: synthetic\n---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: synthetic-release-secret\n  namespace: default\nstringData:\n  password: synthetic-private-value\n"

type helmRunner struct {
	t                *testing.T
	mu               sync.Mutex
	revision         int
	status           string
	failHook         bool
	mutations        int
	calls            []runtimes.Command
	api              *fakeAPI
	currentManifest  string
	retainedManifest string
}

func option(arguments []string, name string) string {
	for index, value := range arguments {
		if value == name && index+1 < len(arguments) {
			return arguments[index+1]
		}
	}
	return ""
}

func (r *helmRunner) Run(_ context.Context, command runtimes.Command) (localtools.Output, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, command)
	if len(command.Args) > 0 && command.Args[0] == "version" {
		return localtools.Output{Stdout: []byte("v4.3.1")}, nil
	}
	if strings.Contains(strings.Join(command.Args, " "), "synthetic-private-value") {
		r.t.Error("Helm received a Secret as a command argument")
	}
	if command.Args[0] != "repo" && command.Args[0] != "pull" {
		if option(command.Args, "--kube-context") != "porto-synthetic" || option(command.Args, "--namespace") != "default" {
			r.t.Error("Helm lost explicit context/namespace")
		}
		path := option(command.Args, "--kubeconfig")
		configuration, err := clientcmd.LoadFromFile(path)
		if err != nil || configuration.CurrentContext != "porto-synthetic" || len(configuration.Contexts) != 1 {
			r.t.Error("Helm inherited an unrelated global kubeconfig")
		}
	}
	output := func(value any) (localtools.Output, error) {
		data, _ := json.Marshal(value)
		return localtools.Output{Stdout: data}, nil
	}
	document := func() map[string]any {
		manifest := r.currentManifest
		if manifest == "" {
			manifest = syntheticHelmManifest
		}
		return map[string]any{"name": "synthetic-release", "namespace": "default", "version": r.revision, "manifest": manifest, "chart": map[string]any{"metadata": map[string]any{"name": "synthetic", "version": "1.0.0", "appVersion": "1"}}, "info": map[string]any{"status": r.status, "last_deployed": "2026-01-01T00:00:00Z"}, "hooks": []any{map[string]any{"name": "synthetic-hook", "kind": "Job", "manifest": "", "last_run": map[string]any{"phase": "Succeeded"}}}}
	}
	switch command.Args[0] {
	case "status":
		if r.revision == 0 {
			return localtools.Output{Stderr: []byte("release: not found")}, errors.New("synthetic not found")
		}
		if r.api != nil {
			r.api.mu.Lock()
			name := "sh.helm.release.v1.synthetic-release.v" + strconv.Itoa(r.revision)
			key := "/secrets/default/" + name
			if r.api.objects[key] == nil {
				r.api.objects[key] = object(r.t, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"`+name+`","namespace":"default","uid":"helm-`+strconv.Itoa(r.revision)+`","resourceVersion":"`+strconv.Itoa(r.revision)+`"}}`)
			}
			r.api.mu.Unlock()
		}
		return output(document())
	case "upgrade":
		if slices.Contains(command.Args, "--dry-run=server") {
			value := document()
			value["version"] = r.revision + 1
			return output(value)
		}
		r.mutations++
		r.revision++
		if r.failHook {
			r.status = "failed"
			return localtools.Output{Stderr: []byte("synthetic hook failed with synthetic-private-value")}, errors.New("synthetic-private-value")
		}
		r.status = "deployed"
		return output(document())
	case "history":
		return output([]any{map[string]any{"revision": 1, "status": "superseded", "chart": "synthetic-1.0.0", "app_version": "1", "updated": "2026-01-01T00:00:00Z", "description": "synthetic-private-value"}})
	case "get":
		if command.Args[1] == "values" {
			return output(map[string]any{"password": "synthetic-private-value"})
		}
		if r.retainedManifest != "" {
			return localtools.Output{Stdout: []byte(r.retainedManifest)}, nil
		}
		return localtools.Output{Stdout: []byte(syntheticHelmManifest)}, nil
	case "rollback":
		r.mutations++
		r.revision++
		r.status = "deployed"
		return localtools.Output{Stdout: []byte("synthetic rollback")}, nil
	case "uninstall":
		if !slices.Contains(command.Args, "--dry-run") {
			r.mutations++
			r.status = "uninstalled"
		}
		return localtools.Output{Stdout: []byte("synthetic uninstall")}, nil
	case "list":
		return output([]any{map[string]any{"name": "synthetic-release", "namespace": "default", "revision": "1", "status": r.status, "chart": "synthetic-1.0.0", "app_version": "1", "updated": "2026-01-01T00:00:00Z"}})
	}
	return localtools.Output{}, errors.New("unexpected synthetic Helm command")
}

func TestHelmRollbackKeepsCurrentAndRetainedStorageAcknowledgement(t *testing.T) {
	const currentPVC = "apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: synthetic-current-data\n  namespace: default\n"
	const retainedPVC = "apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: synthetic-retained-data\n  namespace: default\n"
	for _, test := range []struct {
		name, current, retained string
		warnings                []string
	}{
		{"removed-current-pvc", currentPVC, syntheticHelmManifest, []string{"synthetic-current-data"}},
		{"added-retained-pvc", syntheticHelmManifest, retainedPVC, []string{"synthetic-retained-data"}},
		{"changed-pvc", currentPVC, retainedPVC, []string{"synthetic-current-data", "synthetic-retained-data"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, api, ops := adminFixture(t)
			runner := &helmRunner{t: t, api: api, revision: 2, status: "deployed",
				currentManifest: test.current, retainedManifest: test.retained}
			manager.runner = runner
			request := HelmRequest{HelmScope: HelmScope{Context: "porto-synthetic", Namespace: "default"},
				Release: "synthetic-release", Action: "rollback", Revision: 1, SecretConsent: true}
			preview, err := manager.PreviewHelm(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range test.warnings {
				if !strings.Contains(strings.Join(preview.StorageConsequences, "\n"), name) {
					t.Errorf("rollback omitted the storage consequence for %s: %v", name, preview.StorageConsequences)
				}
			}
			if _, err := manager.ApplyHelm(context.Background(), request.HelmScope, preview.Token, true, false); err == nil {
				t.Fatal("rollback accepted a storage lifecycle change without acknowledgement")
			}
			op, err := manager.ApplyHelm(context.Background(), request.HelmScope, preview.Token, true, true)
			if err != nil {
				t.Fatal(err)
			}
			if result := waitAdmin(t, ops, op.ID); result.Status != "succeeded" {
				t.Fatal(result)
			}
			if runner.mutations != 1 {
				t.Fatalf("rollback acknowledgement did not guard the actual mutation: %d", runner.mutations)
			}
		})
	}
}

func TestHelmLifecyclePinnedScopeHistoryAndSecretRedaction(t *testing.T) {
	manager, api, ops := adminFixture(t)
	runner := &helmRunner{t: t, api: api}
	manager.runner = runner
	chart, err := manager.UploadHelmChart(bytes.NewReader([]byte("synthetic chart archive fixture")))
	if err != nil {
		t.Fatal(err)
	}
	scope := HelmScope{Context: "porto-synthetic", Namespace: "default"}
	request := HelmRequest{HelmScope: scope, Release: "synthetic-release", Action: "upgrade", ChartID: chart, Values: `{"password":"synthetic-private-value"}`, SecretConsent: true}
	preview, err := manager.PreviewHelm(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(preview)
	if bytes.Contains(encoded, []byte("synthetic-private-value")) {
		t.Fatal("Helm Secret escaped a cached preview")
	}
	op, err := manager.ApplyHelm(context.Background(), scope, preview.Token, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitAdmin(t, ops, op.ID); result.Status != "succeeded" {
		t.Fatal(result)
	}
	request.Values = `{"password":"synthetic-new-value"}`
	preview, err = manager.PreviewHelm(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	op, err = manager.ApplyHelm(context.Background(), scope, preview.Token, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitAdmin(t, ops, op.ID); result.Status != "succeeded" {
		t.Fatal(result)
	}
	history, err := manager.HelmHistory(context.Background(), scope, request.Release)
	if err != nil || len(history) == 0 {
		t.Fatal("native Helm history unavailable")
	}
	encoded, _ = json.Marshal(history)
	if bytes.Contains(encoded, []byte("synthetic-private-value")) {
		t.Fatal("Helm history description leaked values")
	}
	if _, err := manager.HelmValues(context.Background(), scope, request.Release, false); err == nil {
		t.Fatal("Helm values were revealed implicitly")
	}
	values, err := manager.HelmValues(context.Background(), scope, request.Release, true)
	if err != nil || !bytes.Contains(values, []byte("synthetic-private-value")) {
		t.Fatal("explicit authorized Helm values reveal did not work")
	}
	request.Action, request.Revision = "rollback", 1
	preview, err = manager.PreviewHelm(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	op, err = manager.ApplyHelm(context.Background(), scope, preview.Token, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitAdmin(t, ops, op.ID); result.Status != "succeeded" {
		t.Fatal(result)
	}
	request.Action = "uninstall"
	preview, err = manager.PreviewHelm(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	op, err = manager.ApplyHelm(context.Background(), scope, preview.Token, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitAdmin(t, ops, op.ID); result.Status != "succeeded" {
		t.Fatal(result)
	}
	if runner.mutations != 4 {
		t.Fatalf("expected one controlled action per lifecycle transition, got %d", runner.mutations)
	}
}

func TestHelmFailedHooksAndConcurrentRevisionAreNotSuccess(t *testing.T) {
	manager, api, ops := adminFixture(t)
	runner := &helmRunner{t: t, revision: 1, status: "deployed", api: api}
	manager.runner = runner
	chart, err := manager.UploadHelmChart(bytes.NewReader([]byte("synthetic chart")))
	if err != nil {
		t.Fatal(err)
	}
	request := HelmRequest{HelmScope: HelmScope{Context: "porto-synthetic", Namespace: "default"}, Release: "synthetic-release", Action: "upgrade", ChartID: chart, Values: "{}", SecretConsent: true}
	preview, err := manager.PreviewHelm(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	runner.revision = 2
	runner.mu.Unlock()
	op, err := manager.ApplyHelm(context.Background(), request.HelmScope, preview.Token, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitAdmin(t, ops, op.ID); result.Status != "failed" {
		t.Fatalf("concurrent release was overwritten: %+v", result)
	}
	preview, err = manager.PreviewHelm(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	runner.failHook = true
	runner.mu.Unlock()
	op, err = manager.ApplyHelm(context.Background(), request.HelmScope, preview.Token, true, true)
	if err != nil {
		t.Fatal(err)
	}
	result := waitAdmin(t, ops, op.ID)
	if result.Status != "partial" || strings.Contains(result.Error, "synthetic-private-value") {
		t.Fatalf("failed hook was hidden or leaked values: %+v", result)
	}
}

func TestNativePinnedHelmRenderingAndLifecycleFlagContract(t *testing.T) {
	bin := os.Getenv("PORTO_TEST_HELM_BIN")
	if bin == "" {
		t.Skip("set PORTO_TEST_HELM_BIN to the verified Helm 4.3.1 bin directory for native CLI acceptance")
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	manager, _, _ := adminFixture(t)
	root := t.TempDir()
	for name, content := range map[string]string{
		"Chart.yaml":            "apiVersion: v2\nname: synthetic\nversion: 1.0.0\n",
		"templates/secret.yaml": "apiVersion: v1\nkind: Secret\nmetadata:\n  name: synthetic-secret\n  namespace: {{ .Release.Namespace }}\nstringData:\n  password: {{ .Values.password | quote }}\n",
	} {
		if err := config.WriteFileAtomic(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	output, err := manager.helm(context.Background(), HelmScope{Context: "porto-synthetic", Namespace: "default"}, []byte(`{"password":"synthetic-private-value"}`), "template", "synthetic-release", root)
	if err != nil || !bytes.Contains(output, []byte("synthetic-private-value")) {
		t.Fatalf("native Helm render failed: %v", err)
	}
	visible, _, err := renderedPublic(string(output))
	if err != nil || bytes.Contains(visible, []byte("synthetic-private-value")) {
		t.Fatal("native Helm Secret render was not redacted")
	}
	for _, command := range []string{"upgrade", "rollback", "uninstall"} {
		output, err := manager.helm(context.Background(), HelmScope{}, nil, command, "--help")
		if err != nil || !bytes.Contains(output, []byte("--wait")) {
			t.Fatalf("native Helm %s flag contract unavailable: %v", command, err)
		}
	}
}
