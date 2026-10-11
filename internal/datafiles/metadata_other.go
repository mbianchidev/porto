//go:build !darwin && !linux

package datafiles

import "os"

func fileMetadata(os.FileInfo) (int, int, int64)        { return 0, 0, -1 }
func fileIdentity(os.FileInfo) string                   { return "" }
func preserveFileOwnership(*os.File, os.FileInfo) error { return nil }
