//go:build windows

package datafiles

import "golang.org/x/sys/windows"

func AvailableSpace(directory string) (uint64, error) {
	name, err := windows.UTF16PtrFromString(directory)
	if err != nil {
		return 0, err
	}
	var available uint64
	if err := windows.GetDiskFreeSpaceEx(name, &available, nil, nil); err != nil {
		return 0, err
	}
	return available, nil
}
