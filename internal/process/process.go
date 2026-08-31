package process

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"
)

func NewCommand(ctx context.Context, dir, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	configure(cmd)
	cmd.Cancel = func() error {
		return Kill(cmd)
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

type Running struct {
	Cmd *exec.Cmd
}

func ShellCommand(ctx context.Context, dir, command string, port int) (*exec.Cmd, io.ReadCloser, io.ReadCloser, error) {
	shell, flag := "/bin/sh", "-c"
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/C"
	}
	cmd, stdout, stderr, err := Command(ctx, dir, shell, flag, command)
	if err != nil {
		return nil, nil, nil, err
	}
	cmd.Env = append(os.Environ(), "PORT="+itoa(port), "PORTO_PORT="+itoa(port))
	return cmd, stdout, stderr, nil
}

func Command(ctx context.Context, dir, name string, args ...string) (*exec.Cmd, io.ReadCloser, io.ReadCloser, error) {
	cmd := NewCommand(ctx, dir, name, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, nil, nil, err
	}
	return cmd, stdout, stderr, nil
}

func Stream(r io.Reader, fn func(string) error) error {
	s := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	s.Buffer(buf, 1024*1024)
	for s.Scan() {
		if err := fn(s.Text()); err != nil {
			return err
		}
	}
	return s.Err()
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	b := [20]byte{}
	i := len(b)
	n := v
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
