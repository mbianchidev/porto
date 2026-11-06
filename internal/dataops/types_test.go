package dataops

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
)

func TestScheduleValidation(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "synthetic backups")
	for _, test := range []struct {
		name   string
		change func(*Schedule)
		valid  bool
	}{
		{name: "new schedule", valid: true},
		{name: "persisted schedule", change: func(s *Schedule) { s.ID = 7 }, valid: true},
		{name: "maximum interval and retention", change: func(s *Schedule) { s.IntervalHours, s.Retention = 8760, 365 }, valid: true},
		{name: "negative identity", change: func(s *Schedule) { s.ID = -1 }},
		{name: "wrong resource kind", change: func(s *Schedule) { s.Resource.Kind = "image" }},
		{name: "missing resource identity", change: func(s *Schedule) { s.Resource.ID = "" }},
		{name: "missing resource name", change: func(s *Schedule) { s.Resource.Name = "" }},
		{name: "read-only volume", change: func(s *Schedule) { s.Resource.ReadOnly = true }},
		{name: "zero interval", change: func(s *Schedule) { s.IntervalHours = 0 }},
		{name: "excessive interval", change: func(s *Schedule) { s.IntervalHours = 8761 }},
		{name: "zero retention", change: func(s *Schedule) { s.Retention = 0 }},
		{name: "excessive retention", change: func(s *Schedule) { s.Retention = 366 }},
		{name: "relative directory", change: func(s *Schedule) { s.Directory = filepath.Join("relative", "backups") }},
		{name: "missing directory", change: func(s *Schedule) { s.Directory = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			schedule := Schedule{
				Resource:      datafiles.Resource{Kind: "volume", Name: "synthetic-volume", ID: "synthetic-generation"},
				IntervalHours: 1,
				Retention:     1,
				Directory:     directory,
			}
			if test.change != nil {
				test.change(&schedule)
			}
			err := schedule.Validate()
			if test.valid {
				if err != nil {
					t.Fatalf("valid schedule rejected: %v", err)
				}
			} else if !errors.Is(err, datafiles.ErrInvalid) {
				t.Fatalf("invalid schedule error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestScheduleDeadlinePreservesPrecisionAndUsesUTC(t *testing.T) {
	now := time.Date(2032, time.December, 31, 23, 30, 1, 123456789, time.FixedZone("synthetic", 2*60*60))
	got := (Schedule{IntervalHours: 3}).Deadline(now)
	if got != "2033-01-01T00:30:01.123456789Z" {
		t.Fatalf("deadline = %q", got)
	}
}

func TestResourceKeysCoordinateOperationsWithoutReusingIdentities(t *testing.T) {
	resource := datafiles.Resource{Kind: "volume", Name: "synthetic-volume", ID: "generation-one"}
	export := Request{Action: "export", Resource: resource}
	restore := Request{Action: "restore", Resource: resource, Destination: "synthetic-destination"}
	if export.ResourceKey() != restore.ResourceKey() || export.ResourceKey() != resource.Fingerprint() {
		t.Fatal("operations on one identified resource do not share its lock")
	}
	recreated := export
	recreated.Resource.ID = "generation-two"
	if recreated.ResourceKey() == export.ResourceKey() {
		t.Fatal("recreated resource reused the previous generation's key")
	}
	for _, action := range []string{"prune", "migration"} {
		if got := (Request{Action: action, Destination: "synthetic-destination"}).ResourceKey(); got != "runtime-data" {
			t.Fatalf("%s key = %q, want the runtime-wide lock", action, got)
		}
	}
	if got := (Request{Action: "import", Destination: "synthetic-destination"}).ResourceKey(); got != "import:synthetic-destination" {
		t.Fatalf("unidentified destination key = %q", got)
	}
}
