package ports

import (
	"fmt"
	"net"
)

func IsFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func Pick(preferred, base int, used map[int]bool) (int, error) {
	if preferred > 0 && !used[preferred] && IsFree(preferred) {
		return preferred, nil
	}
	for port := base; port < base+2000; port++ {
		if !used[port] && IsFree(port) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free port found from %d", base)
}
