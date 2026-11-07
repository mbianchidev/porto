//go:build !darwin && !linux

package etcqlite

import "errors"

func inspectPID(int) (*ProcessIdentity, error) {
	return nil, errors.New("Etcqlite native lifecycle supports macOS and Linux only")
}

func terminatePID(int) error {
	return errors.New("Etcqlite process ownership is unsupported on this platform")
}
func killPID(int) error {
	return errors.New("Etcqlite process ownership is unsupported on this platform")
}
