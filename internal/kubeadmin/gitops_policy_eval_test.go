package kubeadmin

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types/ref"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestGitOpsOwnershipPolicyEvaluatesNativeOwnerAndDelegationCases(t *testing.T) {
	plain := func() map[string]any {
		return map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "synthetic", "namespace": "default", "labels": map[string]any{}, "annotations": map[string]any{}}}
	}
	flux := func(name string) map[string]any {
		x := plain()
		_ = unstructured.SetNestedStringMap(x, map[string]string{"kustomize.toolkit.fluxcd.io/name": name, "kustomize.toolkit.fluxcd.io/namespace": "default"}, "metadata", "labels")
		return x
	}
	helm := func(controller bool) map[string]any {
		x := plain()
		labels := map[string]string{"app.kubernetes.io/managed-by": "Helm"}
		if controller {
			labels["helm.toolkit.fluxcd.io/name"] = "synthetic-release"
			labels["helm.toolkit.fluxcd.io/namespace"] = "default"
		}
		_ = unstructured.SetNestedStringMap(x, labels, "metadata", "labels")
		_ = unstructured.SetNestedStringMap(x, map[string]string{"meta.helm.sh/release-name": "synthetic-release", "meta.helm.sh/release-namespace": "default"}, "metadata", "annotations")
		return x
	}
	staged := func(owner string) map[string]any {
		value := plain()
		_ = unstructured.SetNestedStringMap(value, map[string]string{gitOpsHandoff: "synthetic-handoff", gitOpsHandoffTarget: "flux/default/" + owner}, "metadata", "annotations")
		return value
	}
	for _, entry := range []struct {
		name          string
		before, after any
		allowed       bool
	}{
		{"unowned", nil, plain(), true},
		{"same-flux-owner", flux("first"), flux("first"), true},
		{"competing-flux-owner", flux("first"), flux("second"), false},
		{"unstaged-direct-adoption", plain(), flux("first"), false},
		{"staged-direct-adoption", staged("first"), flux("first"), true},
		{"wrong-staged-owner", staged("first"), flux("second"), false},
		{"native-helm-through-flux", nil, helm(true), true},
		{"direct-helm-handoff", helm(false), helm(true), true},
		{"helm-to-kustomize", helm(true), flux("other"), false},
	} {
		t.Run(entry.name, func(t *testing.T) {
			objects := gitOpsOwnershipObjects(GitOpsScope{Context: "porto-synthetic", Namespace: "default"})
			policy := objects[0]
			env, err := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType), cel.Variable("variables", cel.MapType(cel.StringType, cel.DynType)), cel.Variable("request", cel.DynType))
			if err != nil {
				t.Fatal(err)
			}
			variables := map[string]any{}
			values := map[string]any{"object": entry.after, "oldObject": entry.before, "variables": variables, "request": map[string]any{}}
			evaluate := func(expression string) ref.Val {
				ast, issues := env.Compile(expression)
				if issues != nil && issues.Err() != nil {
					t.Fatal(issues.Err())
				}
				program, err := env.Program(ast)
				if err != nil {
					t.Fatal(err)
				}
				result, _, err := program.Eval(values)
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			declarations, _, _ := unstructured.NestedSlice(policy.Object, "spec", "variables")
			for _, raw := range declarations {
				value := raw.(map[string]any)
				variables[value["name"].(string)] = evaluate(value["expression"].(string))
			}
			conditions, _, _ := unstructured.NestedSlice(policy.Object, "spec", "matchConditions")
			for _, raw := range conditions {
				value := raw.(map[string]any)
				if evaluate(value["expression"].(string)).Value() != true {
					t.Fatal("main resource without a subresource did not match")
				}
			}
			allowed := true
			validations, _, _ := unstructured.NestedSlice(policy.Object, "spec", "validations")
			for _, raw := range validations {
				value := raw.(map[string]any)
				allowed = allowed && evaluate(value["expression"].(string)).Value() == true
			}
			if allowed != entry.allowed {
				t.Fatalf("native ownership policy allowed=%v want=%v", allowed, entry.allowed)
			}
		})
	}
}
