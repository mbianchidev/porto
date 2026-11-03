package kubeadmin

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

func (s *session) gitOpsDependencyGraph(ctx context.Context, request GitOpsRequest) ([]any, []gitOpsGuard, error) {
	if len(request.Dependencies) > 16 {
		return nil, nil, invalid("A GitOps application supports at most 16 explicit same-namespace dependencies.")
	}
	dependencies, guards := []any{}, []gitOpsGuard{}
	visiting, visited, requested := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var visit func(string, int) error
	visit = func(name string, depth int) error {
		if name == request.Name || visiting[name] {
			return conflict("The GitOps dependency graph contains a reconciliation cycle. Break it before mutation.")
		}
		if visited[name] {
			return nil
		}
		if len(validation.IsDNS1123Subdomain(name)) > 0 || depth > 32 || len(visited)+len(visiting) >= 64 {
			return invalid("The GitOps dependency graph is invalid or exceeds its bounded depth/inventory.")
		}
		visiting[name] = true
		target := gitOpsTarget(request.GitOpsScope, request.Kind, name)
		object, err := s.gitOpsRead(ctx, target)
		if err != nil {
			return err
		}
		guards = append(guards, gitOpsGuardFor(target, object))
		nested, _, err := unstructured.NestedSlice(object.Object, "spec", "dependsOn")
		if err != nil {
			return invalid("A native GitOps dependency graph is malformed.")
		}
		for _, raw := range nested {
			dependency, ok := raw.(map[string]any)
			if !ok {
				return invalid("A native GitOps dependency is malformed.")
			}
			namespace, _ := dependency["namespace"].(string)
			if namespace != "" && namespace != request.Namespace {
				return invalid("Cross-namespace GitOps dependencies are not implicit.")
			}
			child, _ := dependency["name"].(string)
			if err := visit(child, depth+1); err != nil {
				return err
			}
		}
		delete(visiting, name)
		visited[name] = true
		return nil
	}
	for _, name := range request.Dependencies {
		if requested[name] {
			return nil, nil, invalid("Explicit GitOps dependencies must be unique.")
		}
		requested[name] = true
		if err := visit(name, 0); err != nil {
			return nil, nil, err
		}
		dependencies = append(dependencies, map[string]any{"name": name})
	}
	return dependencies, guards, nil
}
