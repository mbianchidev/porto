package docker

import (
	"regexp"
	"testing"
)

func TestAllocateDynamicHostPortsAssignsOnlyEngineChosenPorts(t *testing.T) {
	got, err := allocateDynamicHostPorts([]string{"127.0.0.1:8080:80/tcp", "80/tcp", "127.0.0.1::53/udp", "0:3000", "8000-8010:443/tcp"})
	if err != nil {
		t.Fatal(err)
	}
	want := []*regexp.Regexp{
		regexp.MustCompile(`^127\.0\.0\.1:8080:80/tcp$`),
		regexp.MustCompile(`^[1-9][0-9]*:80/tcp$`),
		regexp.MustCompile(`^127\.0\.0\.1:[1-9][0-9]*:53/udp$`),
		regexp.MustCompile(`^[1-9][0-9]*:3000$`),
		regexp.MustCompile(`^8000-8010:443/tcp$`),
	}
	if len(got) != len(want) {
		t.Fatalf("publish = %q", got)
	}
	for index, pattern := range want {
		if !pattern.MatchString(got[index]) {
			t.Fatalf("publish[%d] = %q, want %s", index, got[index], pattern)
		}
	}
	if _, err := allocateDynamicHostPorts([]string{"80/sctp"}); err == nil {
		t.Fatal("automatic SCTP allocation was accepted")
	}
}
