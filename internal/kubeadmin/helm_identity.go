package kubeadmin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (s *session) releaseIdentity(ctx context.Context, scope HelmScope, name string, revision int) (string, error) {
	if revision == 0 {
		return "absent", nil
	}
	target := releaseStore(scope)
	secret := "sh.helm.release.v1." + name + ".v" + strconv.Itoa(revision)
	object, err := s.metadata.Resource(gvr(target)).Namespace(scope.Namespace).Get(ctx, secret, metav1.GetOptions{})
	if err != nil {
		return "", apiError(err)
	}
	if object.UID == "" || object.ResourceVersion == "" {
		return "", conflict("The native Helm release store has no stable UID/resourceVersion. Refresh before continuing.")
	}
	sum := sha256.Sum256([]byte(string(object.UID) + "\n" + object.ResourceVersion))
	return hex.EncodeToString(sum[:]), nil
}
