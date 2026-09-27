package ports

import (
	"net"
	"strings"
	"testing"
)

func TestIsProtocolFreeDetectsOccupiedPorts(t *testing.T) {
	t.Run("tcp", func(t *testing.T) {
		listener, port := listenTCP(t)
		defer listener.Close()

		if IsFree(port) {
			t.Fatalf("TCP port %d reported free while listening", port)
		}
	})

	t.Run("udp", func(t *testing.T) {
		connection, port := listenUDP(t)
		defer connection.Close()

		if IsProtocolFree(port, "udp") {
			t.Fatalf("UDP port %d reported free while listening", port)
		}
	})
}

func TestIsProtocolFreeUsesTCPForUnknownProtocol(t *testing.T) {
	listener, port := listenTCP(t)
	defer listener.Close()

	if IsProtocolFree(port, "sctp") {
		t.Fatalf("unknown protocol did not use TCP availability for port %d", port)
	}
}

func TestPickProtocolUsesPreferredFreePort(t *testing.T) {
	preferred := freeTCPPort(t)

	port, err := PickProtocol(preferred, preferred+1, nil, "tcp")
	if err != nil {
		t.Fatalf("pick preferred port: %v", err)
	}
	if port != preferred {
		t.Fatalf("picked port = %d, want preferred %d", port, preferred)
	}
}

func TestPickProtocolSkipsOccupiedAndUsedPorts(t *testing.T) {
	listener, base := listenTCP(t)
	defer listener.Close()

	used := map[int]bool{base + 1: true}
	port, err := Pick(0, base, used)
	if err != nil {
		t.Fatalf("pick fallback port: %v", err)
	}
	if port <= base+1 {
		t.Fatalf("picked port = %d, want a free port after occupied %d and used %d", port, base, base+1)
	}
	if used[port] {
		t.Fatalf("picked port %d is marked used", port)
	}
}

func TestPickProtocolReportsExhaustedRange(t *testing.T) {
	const base = 30000
	used := make(map[int]bool, 2000)
	for port := base; port < base+2000; port++ {
		used[port] = true
	}

	port, err := PickProtocol(0, base, used, "udp")
	if err == nil {
		t.Fatal("expected exhausted port range error")
	}
	if port != 0 {
		t.Fatalf("exhausted port = %d, want 0", port)
	}
	if !strings.Contains(err.Error(), "no free udp port found from 30000") {
		t.Fatalf("exhausted range error = %q", err)
	}
}

func listenTCP(t *testing.T) (net.Listener, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on TCP loopback: %v", err)
	}
	return listener, listener.Addr().(*net.TCPAddr).Port
}

func listenUDP(t *testing.T) (*net.UDPConn, int) {
	t.Helper()
	connection, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen on UDP loopback: %v", err)
	}
	return connection, connection.LocalAddr().(*net.UDPAddr).Port
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, port := listenTCP(t)
	if err := listener.Close(); err != nil {
		t.Fatalf("release TCP port %d: %v", port, err)
	}
	return port
}
