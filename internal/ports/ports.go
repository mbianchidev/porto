package ports

import (
	"fmt"
	"net"
)

func IsFree(port int) bool {
	return IsProtocolFree(port, "tcp")
}

func IsProtocolFree(port int, protocol string) bool {
	if protocol == "udp" {
		addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}
		conn, err := net.ListenUDP("udp", addr)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func Pick(preferred, base int, used map[int]bool) (int, error) {
	return PickProtocol(preferred, base, used, "tcp")
}

func PickProtocol(preferred, base int, used map[int]bool, protocol string) (int, error) {
	if preferred > 0 && !used[preferred] && IsProtocolFree(preferred, protocol) {
		return preferred, nil
	}
	for port := base; port < base+2000; port++ {
		if !used[port] && IsProtocolFree(port, protocol) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free %s port found from %d", protocol, base)
}
