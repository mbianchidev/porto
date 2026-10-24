//go:build !windows

package daemon

func hostPTYSupported() bool { return true }
