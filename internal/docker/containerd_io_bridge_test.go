package docker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestNonInteractiveLimaBridgeReleasesItsGuestJobsOnControlEOF(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the bridge runs inside a Linux guest; this host probe requires POSIX sh and mkfifo")
	}
	for _, attached := range []bool{false, true} {
		t.Run(map[bool]string{false: "setup failed before FIFO attachment", true: "public stdin closes before output"}[attached], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var bridge runtimes.Process
			runner := &fakeRunner{starter: func(runtimes.Command) (runtimes.Process, error) {
				var err error
				bridge, err = (runtimes.ExecRunner{}).Start(ctx, runtimes.Command{
					Name: "sh", Args: []string{"-c", limaIOBridgeScript, "porto-io-fixture", "0", "0", ""},
					Env: []string{"XDG_RUNTIME_DIR=" + t.TempDir()},
				})
				return bridge, err
			}}
			backend := &grpcContainerRuntime{runner: runner, lima: "synthetic"}
			processIO, err := backend.newLimaProcessIO(ctx, "synthetic", false, false, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = bridge.Kill(); _ = bridge.Wait() })
			stream, err := processIO.creator("synthetic")
			if err != nil {
				t.Fatal(err)
			}
			var writers []*os.File
			if attached {
				for _, path := range []string{stream.Config().Stdout, stream.Config().Stderr} {
					writer, err := os.OpenFile(path, os.O_WRONLY, 0)
					if err != nil {
						t.Fatal(err)
					}
					writers = append(writers, writer)
					t.Cleanup(func() { _ = writer.Close() })
				}
			}
			if err := processIO.stdin.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := processIO.stdin.Write([]byte("unattached input")); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("unattached stdin unexpectedly accepted input: %v", err)
			}
			if attached {
				if _, err := writers[0].Write([]byte("synthetic-output\n")); err != nil {
					t.Fatal(err)
				}
				if output, err := bufio.NewReader(processIO.stdout).ReadString('\n'); err != nil || output != "synthetic-output\n" {
					t.Fatalf("closing unattached stdin terminated active output: %q, %v", output, err)
				}
			}
			closed := make(chan error, 1)
			go func() { _, err := io.Copy(io.Discard, processIO.stdout); closed <- err }()
			if err := bridge.Stdin().Close(); err != nil {
				t.Fatalf("the public stdin closer consumed the private bridge lifetime channel: %v", err)
			}
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("guest FIFO jobs retained the SSH session after private control EOF")
			}
			if err := bridge.Wait(); err != nil {
				t.Fatalf("guest bridge did not exit cleanly after control EOF: %v", err)
			}
			if _, err := os.Stat(stream.Config().Stdout); !os.IsNotExist(err) {
				t.Fatalf("guest FIFO directory was not removed: %v", err)
			}
		})
	}
}

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
