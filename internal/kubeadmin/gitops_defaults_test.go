package kubeadmin

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestGitOpsBootstrapDefaultsAtomicDownwardAPIReferences(t *testing.T) {
	value := object(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"synthetic"},"spec":{"template":{"spec":{"containers":[{"name":"synthetic","env":[{"name":"NAMESPACE","valueFrom":{"fieldRef":{"fieldPath":"metadata.namespace"}}},{"name":"MEMORY","valueFrom":{"resourceFieldRef":{"resource":"limits.memory"}}}]}]}}}}`)
	if err := normalizeGitOpsBootstrapDefaults(value); err != nil {
		t.Fatal(err)
	}

	containers, _, _ := unstructured.NestedSlice(value.Object, "spec", "template", "spec", "containers")
	container := containers[0].(map[string]any)
	environment, _, _ := unstructured.NestedSlice(container, "env")
	first := environment[0].(map[string]any)
	second := environment[1].(map[string]any)
	version, _, _ := unstructured.NestedString(first, "valueFrom", "fieldRef", "apiVersion")
	divisor, _, _ := unstructured.NestedString(second, "valueFrom", "resourceFieldRef", "divisor")
	if version != "v1" || divisor != "1" {
		t.Fatalf("atomic references differ from native defaults: version=%q divisor=%q", version, divisor)
	}
}

func TestGitOpsBootstrapNormalizesDownwardDivisorsAndNetworkProtocols(t *testing.T) {
	value := object(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"synthetic"},"spec":{"template":{"spec":{"containers":[{"name":"synthetic","env":[{"name":"MEMORY","valueFrom":{"resourceFieldRef":{"resource":"limits.memory","divisor":"1000000"}}}]}]}}}}`)
	if err := normalizeGitOpsBootstrapDefaults(value); err != nil {
		t.Fatal(err)
	}
	containers, _, _ := unstructured.NestedSlice(value.Object, "spec", "template", "spec", "containers")
	environment, _, _ := unstructured.NestedSlice(containers[0].(map[string]any), "env")
	divisor, _, _ := unstructured.NestedString(environment[0].(map[string]any), "valueFrom", "resourceFieldRef", "divisor")
	if divisor != "1M" {
		t.Fatalf("divisor was not canonicalized: %s", divisor)
	}
	policy := object(t, `{"apiVersion":"networking.k8s.io/v1","kind":"NetworkPolicy","metadata":{"name":"synthetic"},"spec":{"podSelector":{},"ingress":[{"ports":[{"port":8080}]}]}}`)
	if err := normalizeGitOpsBootstrapDefaults(policy); err != nil {
		t.Fatal(err)
	}
	ingress, _, _ := unstructured.NestedSlice(policy.Object, "spec", "ingress")
	ports, _, _ := unstructured.NestedSlice(ingress[0].(map[string]any), "ports")
	protocol, _, _ := unstructured.NestedString(ports[0].(map[string]any), "protocol")
	if protocol != "TCP" {
		t.Fatal("network protocol did not match the native default")
	}
}
