package environments

import (
	"net/url"
	"testing"
)

func TestGitOpsCapabilitiesSeparateControllerAndApplicationAuthority(t *testing.T) {
	for _, entry := range []struct{ method, path, capability string }{
		{"GET", "/api/kubernetes/gitops/controller", InventoryRead},
		{"GET", "/api/kubernetes/gitops/applications", InventoryRead},
		{"POST", "/api/kubernetes/gitops/previews", WorkloadOperate},
		{"POST", "/api/kubernetes/gitops/apply", WorkloadOperate},
		{"POST", "/api/kubernetes/gitops/controller/previews", InfrastructureOperate},
		{"POST", "/api/kubernetes/gitops/controller/apply", InfrastructureOperate},
		{"POST", "/api/kubernetes/gitops/sources", WorkloadOperate},
		{"DELETE", "/api/kubernetes/gitops/sources/synthetic", WorkloadOperate},
	} {
		actual, err := RequiredCapability(entry.method, entry.path, url.Values{})
		if err != nil || actual != entry.capability {
			t.Fatalf("%s %s: %q %v; want %q", entry.method, entry.path, actual, err, entry.capability)
		}
	}
	if _, err := RequiredCapability("POST", "/api/kubernetes/gitops/controller/arbitrary", url.Values{}); err == nil {
		t.Fatal("unregistered controller action was exposed")
	}
}
