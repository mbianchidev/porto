//go:build !windows

package datafiles

import "syscall"

func nativeNonblockFlag() int { return syscall.O_NONBLOCK }
