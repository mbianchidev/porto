//go:build windows

package docker

import "fmt"

func allocatedFileBytes(path string) (int64, error) {
	return 0, fmt.Errorf("%w: sparse allocation of %s", ErrUnsupported, path)
}
