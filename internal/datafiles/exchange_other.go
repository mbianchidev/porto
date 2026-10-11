//go:build !darwin && !linux

package datafiles

func exchangeDirectories(string, string) error {
	return ErrUnsupported
}
