package deliverysource

import "testing"

func TestGitSourcesRequireConfinedFilesAndExplicitCredentialFreeRefs(t *testing.T) {
	valid := Git{Repository: "https://example.invalid/synthetic/app.git", Ref: "refs/heads/main", Path: "deploy/compose.yaml"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, source := range []Git{
		{Repository: "https://synthetic:secret@example.invalid/app", Ref: "main", Path: "compose.yaml"},
		{Repository: "https://example.invalid/app", Ref: "--upload-pack=bad", Path: "compose.yaml"},
		{Repository: "https://example.invalid/app", Ref: "main", Path: "../compose.yaml"},
		{Repository: "ext::synthetic", Ref: "main", Path: "compose.yaml"},
		{Repository: "https://example.invalid/app", Ref: "", Path: "compose.yaml"},
	} {
		if source.Validate() == nil {
			t.Fatalf("accepted unsafe source: %+v", source)
		}
	}
}
