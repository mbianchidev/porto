package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

const (
	managedRegistrySecretName           = "porto-registry-credentials"
	registryCredentialReconcileInterval = time.Minute
)

type registrySyncState struct {
	Fingerprint string
	SyncedAt    time.Time
}

type registryNamespaceList struct {
	Items []struct {
		Metadata struct {
			Name              string `json:"name"`
			DeletionTimestamp string `json:"deletionTimestamp"`
		} `json:"metadata"`
	} `json:"items"`
}

type registryServiceAccount struct {
	Metadata struct {
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	ImagePullSecrets []struct {
		Name string `json:"name"`
	} `json:"imagePullSecrets"`
}

type registrySecret struct {
	Metadata struct {
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
}

func (p *ClusterProvisioner) SyncRegistryCredentials(
	ctx context.Context,
	clusterName string,
	dockerConfig []byte,
) error {
	if !clusterNamePattern.MatchString(clusterName) {
		return fmt.Errorf("cluster name must match %s", clusterNamePattern)
	}
	request, err := p.readMutableClusterMetadata(clusterName)
	if err != nil {
		return fmt.Errorf("read Kubernetes cluster ownership: %w", err)
	}
	kubeconfigPath := p.clusterKubeconfigPath(clusterName)
	contextName := clusterContextName(request)
	operationContext, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	baseArgs := []string{"--kubeconfig", kubeconfigPath, "--context", contextName}
	run := func(action string, stdin []byte, args ...string) ([]byte, error) {
		commandArgs := append(append([]string(nil), baseArgs...), args...)
		output, err := p.runner.Run(operationContext, runtimes.Command{
			Name:  "kubectl",
			Args:  commandArgs,
			Stdin: stdin,
		})
		if err != nil {
			return output, runtimes.CommandError(action, output, err)
		}
		return output, nil
	}

	output, err := run("list Kubernetes namespaces for registry credentials", nil, "get", "namespaces", "-o", "json")
	if err != nil {
		return err
	}
	var document registryNamespaceList
	if err := json.Unmarshal(output, &document); err != nil {
		return fmt.Errorf("decode Kubernetes namespaces for registry credentials: %w", err)
	}
	namespaces := make([]string, 0, len(document.Items))
	for _, item := range document.Items {
		if item.Metadata.Name == "" || item.Metadata.DeletionTimestamp != "" {
			continue
		}
		namespaces = append(namespaces, item.Metadata.Name)
	}
	sort.Strings(namespaces)
	fingerprint := registrySyncFingerprint(dockerConfig, namespaces)
	p.registrySyncMu.Lock()
	previous := p.registrySync[contextName]
	if previous.Fingerprint == fingerprint &&
		time.Since(previous.SyncedAt) < registryCredentialReconcileInterval {
		p.registrySyncMu.Unlock()
		return nil
	}
	p.registrySyncMu.Unlock()

	var syncErrors []error
	for _, namespace := range namespaces {
		if err := syncNamespaceRegistryCredentials(run, namespace, dockerConfig); err != nil {
			syncErrors = append(syncErrors, err)
		}
	}
	if err := errors.Join(syncErrors...); err != nil {
		return err
	}
	p.registrySyncMu.Lock()
	p.registrySync[contextName] = registrySyncState{
		Fingerprint: fingerprint,
		SyncedAt:    time.Now(),
	}
	p.registrySyncMu.Unlock()
	return nil
}

func syncNamespaceRegistryCredentials(
	run func(string, []byte, ...string) ([]byte, error),
	namespace string,
	dockerConfig []byte,
) error {
	output, err := run(
		"read default service account in namespace "+namespace,
		nil,
		"--namespace", namespace,
		"get", "serviceaccount", "default",
		"-o", "json",
	)
	if err != nil {
		return err
	}
	var account registryServiceAccount
	if err := json.Unmarshal(output, &account); err != nil {
		return fmt.Errorf("decode default service account in namespace %s: %w", namespace, err)
	}
	if account.Metadata.ResourceVersion == "" {
		return fmt.Errorf("default service account in namespace %s has no resource version", namespace)
	}
	var rawAccount map[string]json.RawMessage
	if err := json.Unmarshal(output, &rawAccount); err != nil {
		return fmt.Errorf("decode default service account fields in namespace %s: %w", namespace, err)
	}
	_, imagePullSecretsPresent := rawAccount["imagePullSecrets"]
	names := make([]string, 0, len(account.ImagePullSecrets)+1)
	foundManaged := false
	for _, reference := range account.ImagePullSecrets {
		if reference.Name == "" {
			continue
		}
		if reference.Name == managedRegistrySecretName {
			foundManaged = true
			if len(dockerConfig) == 0 {
				continue
			}
		}
		names = append(names, reference.Name)
	}

	if len(dockerConfig) > 0 {
		if err := ensureRegistrySecretAvailable(run, namespace); err != nil {
			return err
		}
		manifest, err := registrySecretManifest(namespace, dockerConfig)
		if err != nil {
			return err
		}
		if _, err := run(
			"apply registry credentials in namespace "+namespace,
			manifest,
			"apply", "--server-side", "--field-manager", "porto-registry", "-f", "-",
		); err != nil {
			return err
		}
		if !foundManaged {
			names = append(names, managedRegistrySecretName)
		}
	}

	shouldPatch := (len(dockerConfig) > 0 && !foundManaged) || (len(dockerConfig) == 0 && foundManaged)
	if shouldPatch {
		patch, err := registryServiceAccountPatch(
			account.Metadata.ResourceVersion,
			names,
			imagePullSecretsPresent,
		)
		if err != nil {
			return err
		}
		if _, err := run(
			"update default service account in namespace "+namespace,
			nil,
			"--namespace", namespace,
			"patch", "serviceaccount", "default",
			"--type=json",
			"--patch", string(patch),
		); err != nil {
			return err
		}
	}
	if len(dockerConfig) == 0 {
		if _, err := run(
			"remove registry credentials from namespace "+namespace,
			nil,
			"--namespace", namespace,
			"delete", "secret",
			"--selector", "app.kubernetes.io/managed-by=porto,porto.dev/credential-kind=registry",
			"--ignore-not-found=true",
		); err != nil {
			return err
		}
	}
	return nil
}

func ensureRegistrySecretAvailable(
	run func(string, []byte, ...string) ([]byte, error),
	namespace string,
) error {
	output, err := run(
		"inspect registry credential secret in namespace "+namespace,
		nil,
		"--namespace", namespace,
		"get", "secret", managedRegistrySecretName,
		"--ignore-not-found",
		"-o", "json",
	)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(output)) == "" {
		return nil
	}
	var secret registrySecret
	if err := json.Unmarshal(output, &secret); err != nil {
		return fmt.Errorf("decode registry credential secret in namespace %s: %w", namespace, err)
	}
	if secret.Metadata.Labels["app.kubernetes.io/managed-by"] != "porto" ||
		secret.Metadata.Labels["porto.dev/credential-kind"] != "registry" {
		return fmt.Errorf(
			"secret %s already exists in namespace %s and is not managed by Porto",
			managedRegistrySecretName,
			namespace,
		)
	}
	return nil
}

func registrySecretManifest(namespace string, dockerConfig []byte) ([]byte, error) {
	manifest := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      managedRegistrySecretName,
			"namespace": namespace,
			"labels": map[string]string{
				"app.kubernetes.io/managed-by": "porto",
				"porto.dev/credential-kind":    "registry",
			},
		},
		"type": "kubernetes.io/dockerconfigjson",
		"data": map[string]string{
			".dockerconfigjson": base64.StdEncoding.EncodeToString(dockerConfig),
		},
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode registry credential secret for namespace %s: %w", namespace, err)
	}
	return encoded, nil
}

func registryServiceAccountPatch(resourceVersion string, names []string, fieldPresent bool) ([]byte, error) {
	references := make([]map[string]string, 0, len(names))
	for _, name := range names {
		references = append(references, map[string]string{"name": name})
	}
	operation := "add"
	if fieldPresent {
		operation = "replace"
	}
	patch, err := json.Marshal([]map[string]any{
		{
			"op":    "test",
			"path":  "/metadata/resourceVersion",
			"value": resourceVersion,
		},
		{
			"op":    operation,
			"path":  "/imagePullSecrets",
			"value": references,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode default service account registry patch: %w", err)
	}
	return patch, nil
}

func registrySyncFingerprint(dockerConfig []byte, namespaces []string) string {
	hash := sha256.New()
	_, _ = hash.Write(dockerConfig)
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(strings.Join(namespaces, "\x00")))
	return fmt.Sprintf("%x", hash.Sum(nil))
}
