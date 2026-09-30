package runtimefiles

import "testing"

func TestRootlessOwnershipRoundTripsWithoutChangingLogicalUIDs(t *testing.T) {
	descriptor := Descriptor{
		UIDMap: []IDMap{{Namespace: 0, Host: 1000, Size: 1}, {Namespace: 1, Host: 100000, Size: 65535}},
		GIDMap: []IDMap{{Namespace: 0, Host: 1000, Size: 1}, {Namespace: 1, Host: 100000, Size: 65535}},
	}
	uid, gid, err := descriptor.namespaceOwner(100100, 100200)
	if err != nil || uid != 101 || gid != 201 {
		t.Fatalf("logical ownership=%d:%d error=%v", uid, gid, err)
	}
	uid, gid, err = descriptor.hostOwner(uid, gid)
	if err != nil || uid != 100100 || gid != 100200 {
		t.Fatalf("restored ownership=%d:%d error=%v", uid, gid, err)
	}
	if _, _, err := descriptor.namespaceOwner(42, 42); err == nil {
		t.Fatal("unmapped ownership was silently changed")
	}
}
