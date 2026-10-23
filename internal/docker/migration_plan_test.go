package docker

import "testing"

func TestMigrationPlansIgnoreJSONMemberOrderButRetainExactNumbers(t *testing.T) {
	first, err := migrationPlanDigest([]byte(`{"Id":"synthetic","Config":{"Labels":{"one":"1","two":"2"}},"HostConfig":{"NanoCpus":9007199254740993}}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := migrationPlanDigest([]byte(`{"HostConfig":{"NanoCpus":9007199254740993},"Config":{"Labels":{"two":"2","one":"1"}},"Id":"synthetic"}`))
	if err != nil || second != first {
		t.Fatalf("equivalent source metadata invalidated confirmation: %s %s, %v", first, second, err)
	}
	changed, err := migrationPlanDigest([]byte(`{"Id":"synthetic","Config":{"Labels":{"one":"1","two":"2"}},"HostConfig":{"NanoCpus":9007199254740992}}`))
	if err != nil || changed == first {
		t.Fatalf("plan hashing lost an exact source configuration number: %s %s, %v", first, changed, err)
	}
	if _, err := migrationPlanDigest([]byte("synthetic invalid JSON")); err == nil {
		t.Fatal("invalid plan metadata was accepted")
	}
}

func TestMigrationPlanCanonicalizesObservedMountOrderWithoutLosingWriteBoundaries(t *testing.T) {
	first, err := migrationPlanDigest([]byte(`{"Id":"synthetic","Mounts":[{"Destination":"/data","RW":true},{"Destination":"/readonly","RW":false}]}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := migrationPlanDigest([]byte(`{"Mounts":[{"RW":false,"Destination":"/readonly"},{"RW":true,"Destination":"/data"}],"Id":"synthetic"}`))
	if err != nil || first != second {
		t.Fatalf("equivalent mount relationships changed the plan: %s %s, %v", first, second, err)
	}
	changed, err := migrationPlanDigest([]byte(`{"Id":"synthetic","Mounts":[{"Destination":"/readonly","RW":true},{"Destination":"/data","RW":true}]}`))
	if err != nil || changed == first {
		t.Fatalf("mount normalization lost the read-only boundary: %s %s, %v", first, changed, err)
	}
}
