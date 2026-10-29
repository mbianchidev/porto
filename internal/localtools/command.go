package localtools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/process"
	"github.com/mbianchidev/porto/internal/runtimes"
)

const MaxOutput = 64 * 1024 * 1024

type Output struct {
	Stdout []byte
	Stderr []byte
}

type Runner interface {
	Run(context.Context, runtimes.Command) (Output, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, command runtimes.Command) (Output, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := process.NewCommand(ctx, command.Dir, command.Name, command.Args...)
	cmd.Env = process.WithEnvironment(os.Environ(), command.Env...)
	if command.StdinReader != nil {
		cmd.Stdin = command.StdinReader
	} else if command.Stdin != nil {
		cmd.Stdin = bytes.NewReader(command.Stdin)
	}
	stdout := &boundedBuffer{limit: MaxOutput, cancel: cancel}
	stderr := &boundedBuffer{limit: 64 * 1024, cancel: cancel}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	start := time.Now()
	err := cmd.Run()
	slog.Debug("Local tool finished", "binary", command.Name, "duration", time.Since(start), "failed", err != nil)
	if stdout.exceeded || stderr.exceeded {
		err = errors.Join(err, errors.New("local tool exceeded its output limit"))
	}
	return Output{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, err
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	cancel   context.CancelFunc
	exceeded bool
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	if len(value) > b.limit-b.Len() {
		b.exceeded = true
		b.cancel()
		return 0, errors.New("local tool output limit exceeded")
	}
	return b.Buffer.Write(value)
}

func CopyBounded(destination io.Writer, source io.Reader, limit int64) (int64, error) {
	written, err := io.Copy(destination, io.LimitReader(source, limit+1))
	if err != nil {
		return written, err
	}
	if written > limit {
		return written, fmt.Errorf("local data exceeds the %d-byte limit", limit)
	}
	return written, nil
}

func IsolatedEnvironment(home string, additions ...string) []string {
	environment := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "PATH", "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "TMP", "TEMP", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE":
		default:
			environment = append(environment, key+"=")
		}
	}
	environment = append(environment, "HOME="+home, "USERPROFILE="+home, "XDG_CONFIG_HOME="+home, "XDG_CACHE_HOME="+home)
	return append(environment, additions...)
}
