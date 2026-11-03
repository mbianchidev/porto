package kubeadmin

import (
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func normalizeGitOpsBootstrapDefaults(object *unstructured.Unstructured) error {
	normalize := func(mapping map[string]any, fields ...string) error {
		values, found, err := unstructured.NestedMap(mapping, fields...)
		if err != nil {
			return invalid("The pinned installer has invalid resource quantities.")
		}
		if !found {
			return nil
		}
		for key, value := range values {
			quantity, err := resource.ParseQuantity(fmt.Sprint(value))
			if err != nil {
				return invalid("The pinned installer has an invalid native resource quantity.")
			}
			values[key] = quantity.String()
		}
		return unstructured.SetNestedMap(mapping, values, fields...)
	}
	if object.GetKind() == "ResourceQuota" {
		return normalize(object.Object, "spec", "hard")
	}
	if object.GetKind() == "NetworkPolicy" {
		for _, direction := range []string{"ingress", "egress"} {
			rules, found, err := unstructured.NestedSlice(object.Object, "spec", direction)
			if err != nil {
				return invalid("The pinned network policy is invalid.")
			}
			if !found {
				continue
			}
			for _, raw := range rules {
				rule, ok := raw.(map[string]any)
				if !ok {
					return invalid("The pinned network policy rule is invalid.")
				}
				ports, found, err := unstructured.NestedSlice(rule, "ports")
				if err != nil {
					return invalid("The pinned network policy ports are invalid.")
				}
				if !found {
					continue
				}
				for _, raw := range ports {
					port, ok := raw.(map[string]any)
					if !ok {
						return invalid("The pinned network policy port is invalid.")
					}
					if protocol, _ := port["protocol"].(string); protocol == "" {
						port["protocol"] = "TCP"
					}
				}
				if err := unstructured.SetNestedSlice(rule, ports, "ports"); err != nil {
					return err
				}
			}
			if err := unstructured.SetNestedSlice(object.Object, rules, "spec", direction); err != nil {
				return err
			}
		}
	}
	if slices.Contains([]string{"Deployment", "StatefulSet", "DaemonSet"}, object.GetKind()) {
		for _, field := range []string{"containers", "initContainers"} {
			containers, found, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec", field)
			if err != nil {
				return invalid("The pinned controller pod template is invalid.")
			}
			if !found {
				continue
			}
			for _, raw := range containers {
				container, ok := raw.(map[string]any)
				if !ok {
					return invalid("The pinned controller container is invalid.")
				}
				for _, key := range []string{"requests", "limits"} {
					if err := normalize(container, "resources", key); err != nil {
						return err
					}
				}
				environment, found, err := unstructured.NestedSlice(container, "env")
				if err != nil {
					return invalid("The pinned controller environment is invalid.")
				}
				if found {
					for _, raw := range environment {
						entry, ok := raw.(map[string]any)
						if !ok {
							return invalid("The pinned controller environment entry is invalid.")
						}
						if fieldRef, found, _ := unstructured.NestedMap(entry, "valueFrom", "fieldRef"); found {
							if version, _ := fieldRef["apiVersion"].(string); version == "" {
								fieldRef["apiVersion"] = "v1"
							}
							if err := unstructured.SetNestedMap(entry, fieldRef, "valueFrom", "fieldRef"); err != nil {
								return err
							}
						}
						if resourceRef, found, _ := unstructured.NestedMap(entry, "valueFrom", "resourceFieldRef"); found {
							divisor, found, err := unstructured.NestedString(resourceRef, "divisor")
							if err != nil {
								return invalid("The pinned controller resource divisor is invalid.")
							}
							if !found || divisor == "" {
								divisor = "1"
							}
							quantity, err := resource.ParseQuantity(divisor)
							if err != nil {
								return invalid("The pinned controller resource divisor is invalid.")
							}
							resourceRef["divisor"] = quantity.String()
							if err := unstructured.SetNestedMap(entry, resourceRef, "valueFrom", "resourceFieldRef"); err != nil {
								return err
							}
						}
					}
					if err := unstructured.SetNestedSlice(container, environment, "env"); err != nil {
						return err
					}
				}
			}
			if err := unstructured.SetNestedSlice(object.Object, containers, "spec", "template", "spec", field); err != nil {
				return err
			}
		}
	}
	return nil
}
