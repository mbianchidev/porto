//go:build !darwin && !linux

package etcqlite

func requireLocalStorage(string) error { return ErrUnsupported }
