package kubeadmin

import (
	"strings"
	"testing"
)

func TestGitOpsRevisionUsesTheNativeCommitAndArtifactShapes(t *testing.T) {
	sha1 := strings.Repeat("a", 40)
	sha256 := strings.Repeat("b", 64)
	for _, entry := range []struct{ input, want string }{
		{sha1, sha1}, {"sha1:" + sha1, sha1}, {"main@sha1:" + sha1, sha1},
		{sha256, sha256}, {"sha256:" + sha256, sha256}, {"main@sha256:" + sha256, sha256},
		{"1.2.3", "1.2.3"}, {"main", ""}, {"https://synthetic-private-token.example.invalid", ""},
	} {
		if got := gitOpsRevision(entry.input); got != entry.want {
			t.Errorf("%q => %q; want %q", entry.input, got, entry.want)
		}
	}
}
