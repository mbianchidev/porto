//go:build windows

package diagnostics

import "golang.org/x/sys/windows"

func diskCapacity(path string) (uint64, uint64, error) {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var available uint64
	var total uint64
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(pathPointer, &available, &total, &free); err != nil {
		return 0, 0, err
	}
	return available, total, nil
}
