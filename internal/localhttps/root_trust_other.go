//go:build !linux

package localhttps

import "errors"

func RootTrust(string, bool) error {
	return errors.New("privileged certificate trust is only used by systemd Linux; use 'porto cert trust' for the current user's native trust store")
}
