//go:build !linux && !darwin && !windows

package datafiles

import "github.com/pkg/sftp"

func nativeFilesystemStat(string) (*sftp.StatVFS, error) { return nil, ErrUnsupported }
