//go:build windows

package daemon

import "golang.org/x/sys/windows"

func hostPTYSupported() bool {
	return windows.RtlGetVersion().BuildNumber >= 17763
}
