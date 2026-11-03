package kubeadmin

import (
	"context"
	"errors"
	"testing"
)

func TestGitOpsBundleRejectsUnlistedControllerBeforeCacheAccess(t *testing.T) {
	manager, _, _ := adminFixture(t)
	for _, controller := range []string{"", "../../synthetic-private", "flux/../argocd", "unsupported"} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := manager.gitOpsBundle(ctx, controller)
		var known *Error
		if !errors.As(err, &known) || known.Code != 400 {
			t.Fatalf("unlisted controller reached cache/network access: %q: %v", controller, err)
		}
	}
}
