//go:build !linux

package runtimefiles

func resolveOwnership(descriptor Descriptor) (Descriptor, error) { return descriptor, nil }
