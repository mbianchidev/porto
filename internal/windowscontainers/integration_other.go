//go:build !windows

package windowscontainers

import (
	"context"
	"net"
)

func Snapshot(ctx context.Context) Status {
	host, _ := Probe(ctx)
	return statusForHost(host)
}
func Current() (Installation, error)                                   { return Installation{}, ErrUnsupported }
func Action(context.Context, string) error                             { return ErrUnsupported }
func RootAction(context.Context, string, string, string, string) error { return ErrUnsupported }
func RunService(context.Context, string, Serve) error                  { return ErrUnsupported }
func Dial(context.Context, string) (net.Conn, error)                   { return nil, ErrUnsupported }
func dialOwned(context.Context, Installation) (net.Conn, error)        { return nil, ErrUnsupported }
func Listen(Installation) (net.Listener, error)                        { return nil, ErrUnsupported }
func MarkAdmitted(Installation) error                                  { return ErrUnsupported }
func EnsureNetwork(Installation, string, string) (Network, error) {
	return Network{}, ErrUnsupported
}
func CheckNetworks(Installation) error                                      { return ErrUnsupported }
func RemoveNetwork(Installation, string) error                              { return ErrUnsupported }
func Supervise(context.Context, Installation, chan<- struct{}, Serve) error { return ErrUnsupported }
