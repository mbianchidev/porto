//go:build !linux

package main

import "errors"

func runShimLogger([]string) error {
	return errors.New("containerd structured logging requires the Linux guest helper")
}
