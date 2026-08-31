//go:build windows

package docker

import (
	"net"

	"github.com/Microsoft/go-winio"
)

type windowsEndpointLease struct{}

func listenDockerEndpoint(path string) (net.Listener, endpointLease, error) {
	listener, err := winio.ListenPipe(path, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;OW)",
		MessageMode:        false,
		InputBufferSize:    64 * 1024,
		OutputBufferSize:   64 * 1024,
	})
	if err != nil {
		return nil, nil, endpointListenError(path, err)
	}
	return listener, windowsEndpointLease{}, nil
}

func (windowsEndpointLease) Release(string) error {
	return nil
}
