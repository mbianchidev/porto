package networking

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func TestResolverChangesRefreshSplitDNSDiagnosticsWithoutChangingHostSettings(t *testing.T) {
	server, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	var loopback atomic.Bool
	loopback.Store(true)
	go func() {
		data := make([]byte, 4096)
		for {
			n, peer, err := server.ReadFrom(data)
			if err != nil {
				return
			}
			var query dnsmessage.Message
			if query.Unpack(data[:n]) != nil || len(query.Questions) != 1 {
				continue
			}
			answer := dnsmessage.Message{Header: dnsmessage.Header{ID: query.Header.ID, Response: true, Authoritative: true}, Questions: query.Questions}
			question := query.Questions[0]
			if question.Type == dnsmessage.TypeA {
				address := [4]byte{127, 0, 0, 1}
				if !loopback.Load() {
					address = [4]byte{192, 0, 2, 1}
				}
				answer.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: question.Name,
					Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}, Body: &dnsmessage.AResource{A: address}}}
			}
			response, err := answer.Pack()
			if err == nil {
				_, _ = server.WriteTo(response, peer)
			}
		}
	}()
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", server.LocalAddr().String())
	}}
	before, warnings := inspectLocalhost(context.Background(), resolver, nil)
	if len(before) != 1 || before[0] != "127.0.0.1" || len(warnings) != 0 {
		t.Fatalf("initial synthetic resolver: %v %v", before, warnings)
	}
	loopback.Store(false)
	after, warnings := inspectLocalhost(context.Background(), resolver, nil)
	if len(after) != 1 || after[0] != "192.0.2.1" || !strings.Contains(strings.Join(warnings, " "), "non-loopback") {
		t.Fatalf("changed split DNS was not reported: %v %v", after, warnings)
	}
}
