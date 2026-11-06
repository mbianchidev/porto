package localtools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/mbianchidev/porto/internal/process"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestBoundedBufferAllowsExactlyItsLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buffer := &boundedBuffer{limit: 3, cancel: cancel}
	if n, err := buffer.Write([]byte("abc")); n != 3 || err != nil || ctx.Err() != nil {
		t.Fatalf("exact-limit write = %d, %v; cancellation = %v", n, err, ctx.Err())
	}
	if n, err := buffer.Write([]byte("d")); n != 0 || err == nil || !buffer.exceeded || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("overflow write = %d, %v; exceeded = %t, cancellation = %v", n, err, buffer.exceeded, ctx.Err())
	}
}

func TestBoundedBufferEnforcesLimitForStreamCopies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buffer := &boundedBuffer{limit: 3, cancel: cancel}
	n, err := io.Copy(buffer, iotest.OneByteReader(strings.NewReader("abcd")))
	if n != 3 || err == nil || !buffer.exceeded || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("stream copy = %d, %v; exceeded = %t, cancellation = %v", n, err, buffer.exceeded, ctx.Err())
	}
}

func TestCopyBoundedReportsOversizeAndReadFailures(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		limit int64
		fail  bool
	}{
		{name: "empty", limit: 0},
		{name: "below limit", input: "ab", limit: 3},
		{name: "exact limit", input: "abc", limit: 3},
		{name: "overflow", input: "abcdef", limit: 3, fail: true},
		{name: "zero limit", input: "a", limit: 0, fail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			n, err := CopyBounded(io.Discard, strings.NewReader(test.input), test.limit)
			want := min(int64(len(test.input)), test.limit+1)
			if n != want || (err != nil) != test.fail {
				t.Fatalf("copy = %d, %v; want %d bytes, failure=%t", n, err, want, test.fail)
			}
		})
	}
	failure := errors.New("synthetic read failure")
	if _, err := CopyBounded(io.Discard, iotest.ErrReader(failure), 3); !errors.Is(err, failure) {
		t.Fatalf("reader failure was lost: %v", err)
	}
}

func TestIsolatedEnvironmentClearsAmbientValuesAndAllowsExplicitAdditions(t *testing.T) {
	t.Setenv("PORTO_LOCALTOOLS_SYNTHETIC_SECRET", "synthetic-secret")
	t.Setenv("PATH", "synthetic-path")
	home := t.TempDir()
	values := map[string]string{}
	for _, entry := range process.WithEnvironment(os.Environ(), IsolatedEnvironment(home, "PORTO_LOCALTOOLS_ALLOWED=synthetic-allowed")...) {
		key, value, _ := strings.Cut(entry, "=")
		values[strings.ToUpper(key)] = value
	}
	if values["PORTO_LOCALTOOLS_SYNTHETIC_SECRET"] != "" || values["PATH"] != "synthetic-path" || values["PORTO_LOCALTOOLS_ALLOWED"] != "synthetic-allowed" {
		t.Fatal("isolated environment leaked an ambient value or lost an explicit value")
	}
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		if values[key] != home {
			t.Fatalf("%s was not redirected to the isolated home", key)
		}
	}
}

func localToolCommand(t *testing.T, mode string) runtimes.Command {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return runtimes.Command{
		Name: executable,
		Args: []string{"-test.run=^TestLocalToolHelperProcess$"},
		Env:  []string{"PORTO_LOCALTOOLS_TEST_HELPER=" + mode},
	}
}

func TestExecRunnerPreservesInputStreamsEnvironmentAndWorkdir(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("reader=%t", stream), func(t *testing.T) {
			command := localToolCommand(t, "echo")
			command.Dir = t.TempDir()
			if err := os.WriteFile(filepath.Join(command.Dir, "synthetic-input.txt"), []byte("synthetic-file"), 0o600); err != nil {
				t.Fatal(err)
			}
			command.Env = append(command.Env, "PORTO_LOCALTOOLS_TEST_VALUE=synthetic-value")
			command.Stdin = []byte("synthetic-input")
			if stream {
				command.Stdin = []byte("must-not-be-read")
				command.StdinReader = strings.NewReader("synthetic-input")
			}
			output, err := (ExecRunner{}).Run(context.Background(), command)
			if err != nil || string(output.Stdout) != "synthetic-value:synthetic-file:synthetic-input" || string(output.Stderr) != "synthetic-diagnostic" {
				t.Fatalf("tool result = %q, %q, %v", output.Stdout, output.Stderr, err)
			}
		})
	}
}

func TestExecRunnerRetainsFailureDiagnosticsAndExitCode(t *testing.T) {
	output, err := (ExecRunner{}).Run(context.Background(), localToolCommand(t, "fail"))
	var exited interface{ ExitCode() int }
	if !errors.As(err, &exited) || exited.ExitCode() != 37 || string(output.Stdout) != "synthetic-partial-output" || string(output.Stderr) != "synthetic-failure" {
		t.Fatalf("failed tool result = %q, %q, %v", output.Stdout, output.Stderr, err)
	}
}

func TestExecRunnerRejectsExcessiveDiagnostics(t *testing.T) {
	output, err := (ExecRunner{}).Run(context.Background(), localToolCommand(t, "oversized-stderr"))
	if err == nil || !strings.Contains(err.Error(), "output limit") || len(output.Stderr) > 64*1024 {
		t.Fatalf("oversized tool result = %d stderr bytes, %v", len(output.Stderr), err)
	}
}

func TestExecRunnerHonorsCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (ExecRunner{}).Run(ctx, localToolCommand(t, "echo")); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation was lost: %v", err)
	}
}

func TestLocalToolHelperProcess(t *testing.T) {
	switch os.Getenv("PORTO_LOCALTOOLS_TEST_HELPER") {
	case "":
		return
	case "echo":
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile("synthetic-input.txt")
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(os.Stdout, "%s:%s:%s", os.Getenv("PORTO_LOCALTOOLS_TEST_VALUE"), content, input)
		fmt.Fprint(os.Stderr, "synthetic-diagnostic")
	case "fail":
		fmt.Fprint(os.Stdout, "synthetic-partial-output")
		fmt.Fprint(os.Stderr, "synthetic-failure")
		os.Exit(37)
	case "oversized-stderr":
		fmt.Fprint(os.Stderr, strings.Repeat("x", 64*1024+1))
	default:
		t.Fatal("unknown synthetic local-tool mode")
	}
	os.Exit(0)
}
