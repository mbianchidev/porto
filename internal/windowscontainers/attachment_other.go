//go:build !windows

package windowscontainers

import "context"

func AttachNetwork(context.Context, Installation, string, string, []string) (Attachment, error) {
	return Attachment{}, ErrUnsupported
}
func DetachNetwork(context.Context, Installation, Attachment) error { return ErrUnsupported }
