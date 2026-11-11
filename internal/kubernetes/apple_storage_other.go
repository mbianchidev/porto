//go:build !darwin

package kubernetes

import "errors"

func appleBackingFileIdentity(string) (string, error) {
	return "", errors.New("native backing storage identities require macOS")
}
