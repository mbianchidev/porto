//go:build !darwin && !linux

package syncshares

import "errors"

func directoryIdentity(string) (string, error) {
	return "", errors.New("optimized synchronized sharing is unsupported on this host")
}
