package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/mbianchidev/porto/internal/dataops"
)

func TestSourceHelperReservationsSurviveRestartAndKeepCleanupOrder(t *testing.T) {
	database := filepath.Join(t.TempDir(), "fixture.db")
	st, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	lease := dataops.SourceTemporary{Context: "fixture-context", Endpoint: "npipe:////./pipe/fixture", Owner: "fixture-owner", Name: "fixture-helper"}
	for _, kind := range []string{"image", "container"} {
		lease.Kind = kind
		if err := st.ReserveMigrationTemporary(context.Background(), lease); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	resources, err := st.MigrationTemporaries(context.Background())
	if err != nil || len(resources) != 2 || resources[0].Kind != "container" {
		t.Fatalf("durable source cleanup order=%+v error=%v", resources, err)
	}
	for _, resource := range resources {
		if err := st.ClearMigrationTemporary(context.Background(), resource); err != nil {
			t.Fatal(err)
		}
	}
	resources, err = st.MigrationTemporaries(context.Background())
	if err != nil || len(resources) != 0 {
		t.Fatalf("cleared cleanup reservations remained: %+v %v", resources, err)
	}
}
