package kubernetes

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/vm"
)

func TestSyncRegistryCredentialsAppliesEveryNamespaceAndPreservesExistingSecrets(t *testing.T) {
	runner := newFakeRunner()
	var appliedNamespaces []string
	var patches []string
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		joined := strings.Join(command.Args, " ")
		switch {
		case strings.Contains(joined, "get namespaces -o json"):
			return []byte(`{"items":[{"metadata":{"name":"default"}},{"metadata":{"name":"dev"}},{"metadata":{"name":"old","deletionTimestamp":"2026-09-13T10:00:00Z"}}]}`), nil
		case strings.Contains(joined, "--namespace default get serviceaccount default"):
			return []byte(`{"metadata":{"resourceVersion":"10"},"imagePullSecrets":[{"name":"existing"}]}`), nil
		case strings.Contains(joined, "--namespace dev get serviceaccount default"):
			return []byte(`{"metadata":{"resourceVersion":"11"},"imagePullSecrets":[{"name":"porto-registry-credentials"}]}`), nil
		case strings.Contains(joined, "apply --server-side"):
			var manifest struct {
				Metadata struct {
					Namespace string `json:"namespace"`
				} `json:"metadata"`
				Data map[string]string `json:"data"`
			}
			if err := json.Unmarshal(command.Stdin, &manifest); err != nil {
				t.Fatalf("decode registry secret: %v", err)
			}
			if manifest.Data[".dockerconfigjson"] == "" {
				t.Fatal("registry secret did not contain Docker config data")
			}
			appliedNamespaces = append(appliedNamespaces, manifest.Metadata.Namespace)
			return nil, nil
		case strings.Contains(joined, "patch serviceaccount default"):
			patches = append(patches, joined)
			return nil, nil
		default:
			return nil, nil
		}
	}
	root := t.TempDir()
	provisioner := NewClusterProvisioner(vm.New(runner), runner, root)
	if err := os.WriteFile(
		provisioner.clusterMetadataPath("dev"),
		[]byte(`{"name":"dev","provider":"k3s"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(provisioner.clusterKubeconfigPath("dev"), []byte(`{"current-context":"porto-k3s-dev"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config := []byte(`{"auths":{"ghcr.io":{"auth":"dGVzdDp0b2tlbg=="}}}`)

	if err := provisioner.SyncRegistryCredentials(context.Background(), "dev", config); err != nil {
		t.Fatal(err)
	}
	if strings.Join(appliedNamespaces, ",") != "default,dev" {
		t.Fatalf("applied namespaces = %v", appliedNamespaces)
	}
	if len(patches) != 1 ||
		!strings.Contains(patches[0], `existing`) ||
		!strings.Contains(patches[0], managedRegistrySecretName) ||
		!strings.Contains(patches[0], `"op":"test"`) ||
		!strings.Contains(patches[0], `"value":"10"`) {
		t.Fatalf("service account patches = %v", patches)
	}

	commandCount := len(runner.commands)
	if err := provisioner.SyncRegistryCredentials(context.Background(), "dev", config); err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != commandCount+1 {
		t.Fatalf("cached sync ran %d extra commands, want namespace check only", len(runner.commands)-commandCount)
	}
}

func TestSyncRegistryCredentialsRemovesManagedReference(t *testing.T) {
	runner := newFakeRunner()
	var patch string
	deleted := false
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		joined := strings.Join(command.Args, " ")
		switch {
		case strings.Contains(joined, "get namespaces -o json"):
			return []byte(`{"items":[{"metadata":{"name":"default"}}]}`), nil
		case strings.Contains(joined, "get serviceaccount default"):
			return []byte(`{"metadata":{"resourceVersion":"20"},"imagePullSecrets":[{"name":"existing"},{"name":"porto-registry-credentials"}]}`), nil
		case strings.Contains(joined, "delete secret --selector"):
			deleted = true
			return nil, nil
		case strings.Contains(joined, "patch serviceaccount default"):
			patch = joined
			return nil, nil
		default:
			return nil, nil
		}
	}
	root := t.TempDir()
	provisioner := NewClusterProvisioner(vm.New(runner), runner, root)
	if err := os.WriteFile(
		provisioner.clusterMetadataPath("dev"),
		[]byte(`{"name":"dev","provider":"kind"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(provisioner.clusterKubeconfigPath("dev"), []byte(`{"current-context":"porto-dev"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := provisioner.SyncRegistryCredentials(context.Background(), "dev", nil); err != nil {
		t.Fatal(err)
	}
	if !deleted || !strings.Contains(patch, `existing`) || strings.Contains(patch, managedRegistrySecretName) {
		t.Fatalf("delete = %t, patch = %q", deleted, patch)
	}
}

func TestSyncRegistryCredentialsDoesNotOverwriteUserOwnedSecret(t *testing.T) {
	runner := newFakeRunner()
	applied := false
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		joined := strings.Join(command.Args, " ")
		switch {
		case strings.Contains(joined, "get namespaces -o json"):
			return []byte(`{"items":[{"metadata":{"name":"default"}}]}`), nil
		case strings.Contains(joined, "get serviceaccount default"):
			return []byte(`{"metadata":{"resourceVersion":"30"}}`), nil
		case strings.Contains(joined, "get secret porto-registry-credentials"):
			return []byte(`{"metadata":{"labels":{"app.kubernetes.io/managed-by":"someone-else"}}}`), nil
		case strings.Contains(joined, "apply --server-side"):
			applied = true
			return nil, nil
		default:
			return nil, nil
		}
	}
	root := t.TempDir()
	provisioner := NewClusterProvisioner(vm.New(runner), runner, root)
	if err := os.WriteFile(
		provisioner.clusterMetadataPath("dev"),
		[]byte(`{"name":"dev","provider":"k0s"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(provisioner.clusterKubeconfigPath("dev"), []byte(`{"current-context":"porto-dev"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	err := provisioner.SyncRegistryCredentials(
		context.Background(),
		"dev",
		[]byte(`{"auths":{"ghcr.io":{"auth":"dGVzdDp0b2tlbg=="}}}`),
	)
	if err == nil || !strings.Contains(err.Error(), "is not managed by Porto") {
		t.Fatalf("user-owned secret error = %v", err)
	}
	if applied {
		t.Fatal("user-owned registry secret was overwritten")
	}
}
