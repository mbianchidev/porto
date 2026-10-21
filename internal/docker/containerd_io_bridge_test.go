package docker

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestProcessInputClosureIsIdempotentAndPreservesBytes(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	finished := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(reader)
		finished <- data
	}()
	closes := 0
	input := &processInputCloser{WriteCloser: writer, finish: func() error {
		closes++
		return writer.Close()
	}}
	if _, err := input.Write([]byte("synthetic-input")); err != nil {
		t.Fatal(err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	if data := <-finished; string(data) != "synthetic-input" || closes != 1 {
		t.Fatalf("input=%q, close calls=%d", data, closes)
	}
}

func TestLimaIOBridgeForwardsStdinToGuestFIFO(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the bridge runs inside a Linux guest; this host probe requires POSIX sh and mkfifo")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "sh", "-c", limaIOBridgeScript, "porto-io-fixture", "0", "1", "")
	command.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+t.TempDir())
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = input.Close()
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	ready, err := bufio.NewReader(output).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(strings.TrimSpace(ready), "\t")
	if len(fields) != 4 || fields[0] != "PORTO_IO" {
		t.Fatalf("unexpected bridge handshake %q", ready)
	}
	marker := []byte("synthetic-kind-configuration\n")
	if _, err := input.Write(marker); err != nil {
		t.Fatal(err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	stdinFIFO, err := os.Open(fields[1])
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(stdinFIFO)
	closeErr := stdinFIFO.Close()
	for _, filename := range fields[2:] {
		file, err := os.OpenFile(filename, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	waitErr := command.Wait()
	if readErr != nil || closeErr != nil || waitErr != nil || !bytes.Equal(data, marker) {
		t.Fatalf("guest FIFO data=%q, want=%q, read=%v close=%v wait=%v stderr=%q", data, marker, readErr, closeErr, waitErr, diagnostic.String())
	}
}
