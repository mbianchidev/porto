package kubeadmin

import "testing"

func TestGitOpsNewPrivateSourceCannotSilentlyBecomePublic(t *testing.T) {
	manager, _, _, scope := gitOpsFixture(t, "flux")
	if _, err := manager.SaveGitOpsSource(GitOpsSourceRequest{GitOpsScope: scope, Name: "synthetic-private", Kind: "git", URL: "https://git.example.invalid/synthetic.git", Username: "synthetic"}); err == nil {
		t.Fatal("a new private source without a credential silently became public")
	}
}
