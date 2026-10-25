package runtimes

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/term"
)

func TestRuntimePTYChild(t *testing.T) {
	mode := os.Getenv("PORTO_TEST_RUNTIME_PTY_CHILD")
	if mode == "" {
		return
	}
	fmt.Println("runtime-pty-ready")
	if mode == "quiet" {
		for {
			time.Sleep(time.Second)
		}
	}
	reader := bufio.NewScanner(os.Stdin)
	for reader.Scan() {
		switch strings.TrimSpace(reader.Text()) {
		case "size":
			w, h, err := term.GetSize(int(os.Stdout.Fd()))
			fmt.Printf("runtime-pty-size:%dx%d:%v\n", w, h, err)
		case "exit":
			fmt.Println("runtime-pty-final-output")
			os.Exit(7)
		}
	}
	os.Exit(0)
}

func startTestPTYProcess(t *testing.T, ctx context.Context, mode string) (Process, <-chan string) {
	t.Helper()
	process, err := (ExecRunner{}).StartPTY(ctx, Command{Name: os.Args[0], Args: []string{"-test.run=^TestRuntimePTYChild$"},
		Env: []string{"PORTO_TEST_RUNTIME_PTY_CHILD=" + mode}}, [2]uint{30, 90})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Kill(); _ = process.Wait() })
	output := make(chan string, 64)
	go func() {
		defer close(output)
		data := make([]byte, 4096)
		for {
			n, err := process.Stdout().Read(data)
			if n > 0 {
				output <- string(data[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return process, output
}

func waitPTYText(t *testing.T, ctx context.Context, output <-chan string, want string) string {
	t.Helper()
	var text strings.Builder
	for {
		select {
		case chunk, ok := <-output:
			if !ok {
				t.Fatalf("PTY closed before %q: %s", want, text.String())
			}
			text.WriteString(chunk)
			if strings.Contains(text.String(), want) {
				return text.String()
			}
		case <-ctx.Done():
			t.Fatalf("PTY did not produce %q: %s", want, text.String())
		}
	}
}

func TestPTYProcessPreservesResizeOutputAndExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	process, output := startTestPTYProcess(t, ctx, "echo")
	waitPTYText(t, ctx, output, "runtime-pty-ready")
	if _, err := process.Stdin().Write([]byte("size\r\n")); err != nil {
		t.Fatal(err)
	}
	waitPTYText(t, ctx, output, "runtime-pty-size:90x30")
	resize := process.(interface {
		Resize(context.Context, uint32, uint32) error
	})
	if err := resize.Resize(ctx, 132, 41); err != nil {
		t.Fatal(err)
	}
	if _, err := process.Stdin().Write([]byte("size\r\n")); err != nil {
		t.Fatal(err)
	}
	waitPTYText(t, ctx, output, "runtime-pty-size:132x41")
	if err := resize.Resize(ctx, 32768, 1); err == nil {
		t.Fatal("unrepresentable ConPTY width was accepted")
	}
	if _, err := process.Stdin().Write([]byte("exit\r\n")); err != nil {
		t.Fatal(err)
	}
	waitPTYText(t, ctx, output, "runtime-pty-final-output")
	for range output {
	}
	var exited *exec.ExitError
	if err := process.Wait(); !errors.As(err, &exited) || exited.ExitCode() != 7 {
		t.Fatalf("PTY wait lost exit 7: %v", err)
	}
	if process.(*ptyProcess).command.ProcessState == nil {
		t.Fatal("PTY child was not reaped")
	}
}

func TestPTYProcessCancellationReapsQuietChildWithoutAReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	process, err := (ExecRunner{}).StartPTY(ctx, Command{Name: os.Args[0], Args: []string{"-test.run=^TestRuntimePTYChild$"},
		Env: []string{"PORTO_TEST_RUNTIME_PTY_CHILD=quiet"}}, [2]uint{})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled PTY reported success")
		}
		if process.(*ptyProcess).command.ProcessState == nil {
			t.Fatal("cancelled child was not reaped")
		}
	case <-time.After(5 * time.Second):
		_ = process.Kill()
		t.Fatal("quiet PTY cancellation did not finish")
	}
}

func TestPTYProcessInputDisconnectOnlyReapsItsChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, firstOutput := startTestPTYProcess(t, ctx, "quiet")
	second, secondOutput := startTestPTYProcess(t, ctx, "echo")
	waitPTYText(t, ctx, firstOutput, "runtime-pty-ready")
	waitPTYText(t, ctx, secondOutput, "runtime-pty-ready")
	first.(interface{ InputEOF() }).InputEOF()
	if err := first.Wait(); err == nil {
		t.Fatal("disconnected process reported a natural exit")
	}
	if first.(*ptyProcess).command.ProcessState == nil {
		t.Fatal("disconnected process was not reaped")
	}
	if _, err := second.Stdin().Write([]byte("size\r\n")); err != nil {
		t.Fatal(err)
	}
	waitPTYText(t, ctx, secondOutput, "runtime-pty-size:90x30")
}

func TestPTYProcessInvalidRequestsNeverSpawn(t *testing.T) {
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	for _, test := range []struct {
		ctx     context.Context
		command Command
		size    [2]uint
	}{
		{context.Background(), Command{Name: "must-not-spawn"}, [2]uint{32768, 80}},
		{cancelled, Command{Name: "must-not-spawn"}, [2]uint{}},
		{context.Background(), Command{Name: "must-not-spawn", Stdin: []byte("not streaming")}, [2]uint{}},
	} {
		if process, err := (ExecRunner{}).StartPTY(test.ctx, test.command, test.size); err == nil || process != nil {
			t.Fatalf("invalid PTY request spawned: %v, %v", process, err)
		}
	}
}

var _ io.ReadWriter = (*PTY)(nil)
