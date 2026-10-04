package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/dataops"
)

func TestPrunePreviewProtectsOwnersAndRevalidatesIdentity(t *testing.T) {
	manager := New(&fakeRunner{})
	unused := StorageResource{
		Resource: datafiles.Resource{Kind: "volume", Name: "unused-fixture", ID: "fixture-volume-1"},
	}
	owned := StorageResource{
		Resource: datafiles.Resource{Kind: "volume", Name: "owned-fixture", ID: "fixture-volume-2"},
		InUse:    true, Owners: []StorageOwner{{ID: "fixture-container", ComposeProject: "fixture-compose"}},
	}
	finishStorageResource(manager, &unused)
	finishStorageResource(manager, &owned)
	manager.storageReader = func(context.Context) (StorageUsage, error) {
		return StorageUsage{Resources: []StorageResource{unused, owned}}, nil
	}
	request := dataops.Request{Action: "prune", Categories: []string{"volume"}}
	first, err := manager.PreviewPrune(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Candidates) != 1 || len(first.Excluded) != 1 || first.UpperBound != nil {
		t.Fatalf("unsafe or fabricated prune preview: %+v", first)
	}
	unused.Resource.ID = "recreated-fixture-volume"
	finishStorageResource(manager, &unused)
	second, err := manager.PreviewPrune(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == second.Token {
		t.Fatal("a reused volume name did not invalidate the preview")
	}
	if _, err := manager.PreviewPrune(context.Background(), dataops.Request{}); err == nil {
		t.Fatal("broad cleanup became the default")
	}
}

func TestDockerSystemDiskUsageReportsUniqueStoresWithoutInventingTotals(t *testing.T) {
	manager := New(&fakeRunner{})
	size := int64(1024)
	manager.storageReader = func(context.Context) (StorageUsage, error) {
		return StorageUsage{
			ContentBytes: 1024, SnapshotBytes: 2048,
			Resources: []StorageResource{{Resource: datafiles.Resource{Kind: "image", ID: "fixture-image", Name: "fixture:latest"}, LogicalBytes: &size, SharedBytes: &size}},
			Warnings:  []string{"Metadata allocation unavailable"}, Accounting: "namespace-unique",
		}, nil
	}
	response := httptest.NewRecorder()
	NewAPI(manager, "/tmp/fixture.sock").ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1.47/system/df", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("disk usage status=%d: %s", response.Code, response.Body.String())
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if string(result["LayersSize"]) != "1024" || result["Warnings"] == nil || result["PortoAccounting"] == nil {
		t.Fatalf("accounting provenance missing: %s", response.Body.String())
	}
	if _, fabricated := result["TotalBytes"]; fabricated {
		t.Fatal("a shared-store grand total was fabricated")
	}
}
