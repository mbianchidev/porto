//go:build !darwin && !linux && !windows

package datafiles

func AvailableSpace(string) (uint64, error) { return 0, ErrUnsupported }
