package environments

import (
	"slices"
	"testing"
)

func TestMetadataChangesPreserveTargetIdentityAndRejectStaleEdits(t *testing.T) {
	manager := testManager(t)
	agent, remote := testAgent(t, "synthetic-metadata")
	profile := enrollHost(t, manager, agent, remote, "original")
	updated, err := manager.Metadata(profile.ID, MetadataRequest{Name: "Renamed host", Labels: map[string]string{"group": "synthetic-fleet"}, Revision: profile.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != profile.ID || updated.Identity != profile.Identity || updated.Address != profile.Address || !slices.Equal(updated.Capabilities, profile.Capabilities) || updated.Owned != profile.Owned {
		t.Fatal("descriptive metadata retargeted or elevated an environment")
	}
	if updated.Revision <= profile.Revision || updated.Labels["group"] != "synthetic-fleet" {
		t.Fatal("metadata update did not invalidate old fleet previews")
	}
	if _, err := manager.Metadata(profile.ID, MetadataRequest{Name: "stale", Labels: map[string]string{}, Revision: profile.Revision}); err == nil {
		t.Fatal("stale metadata edit overwrote concurrent state")
	}
}
