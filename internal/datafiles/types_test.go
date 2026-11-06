package datafiles

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestResourceFingerprintCompatibility(t *testing.T) {
	for _, test := range []struct {
		name     string
		document string
	}{
		{
			name:     "optional identity fields omitted",
			document: `{"kind":"volume","name":"synthetic-volume","id":"synthetic-generation","readOnly":false}`,
		},
		{
			name:     "creation time retained",
			document: `{"kind":"volume","name":"synthetic-volume","id":"synthetic-generation","createdAt":"2026-01-01T00:00:00Z","readOnly":false}`,
		},
		{
			name:     "backend retained",
			document: `{"kind":"volume","name":"synthetic-volume","id":"synthetic-generation","backend":"synthetic-backend","readOnly":false}`,
		},
		{
			name:     "complete read-only identity",
			document: `{"kind":"image","name":"synthetic:latest","id":"synthetic-generation","createdAt":"2026-01-01T00:00:00Z","backend":"synthetic-backend","readOnly":true}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var resource Resource
			if err := json.Unmarshal([]byte(test.document), &resource); err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte(test.document))
			if got, want := resource.Fingerprint(), hex.EncodeToString(hash[:]); got != want {
				t.Fatalf("persisted resource fingerprint changed: got %s, want %s for %s", got, want, test.document)
			}
		})
	}
}
