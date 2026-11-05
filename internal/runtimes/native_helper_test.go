package runtimes

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type nativeHelperStreamingRunner struct {
	run func(context.Context, Command, func(OutputChunk) error) ([]byte, error)
}

func (nativeHelperStreamingRunner) Run(context.Context, Command) ([]byte, error) {
	return nil, errors.New("native helper installation must stream its input")
}

func (runner nativeHelperStreamingRunner) RunStreaming(ctx context.Context, command Command, emit func(OutputChunk) error) ([]byte, error) {
	return runner.run(ctx, command, emit)
}

func nativeHelperBundle(t *testing.T, data []byte) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "synthetic native helper bundle")
	helper := filepath.Join(directory, "runtime", "bin", "porto-runtime-helper")
	if err := os.MkdirAll(filepath.Dir(helper), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(directory, "porto")
}

func TestInstallNativeHelperStreamsBinaryThroughPipe(t *testing.T) {
	data := bytes.Repeat([]byte{0, 26, 13, 10, 255, 1, 7}, 16384)
	executable := nativeHelperBundle(t, data)
	checksum := sha256.Sum256(data)
	digest := hex.EncodeToString(checksum[:])
	command := Command{
		Name: "limactl",
		Args: []string{"shell", "--workdir=/", "synthetic-vm", "--"},
		Dir:  filepath.Dir(executable),
		Env:  []string{"PORTO_SYNTHETIC_HELPER_ENV=1"},
	}
	streamed := false
	runner := nativeHelperStreamingRunner{run: func(ctx context.Context, actual Command, emit func(OutputChunk) error) ([]byte, error) {
		streamed = true
		expected := append(slices.Clone(command.Args), "sh", "-c", installNativeHelperScript, "porto-native-helper", digest)
		if actual.Name != command.Name || actual.Dir != command.Dir ||
			!slices.Equal(actual.Env, command.Env) || !slices.Equal(actual.Args, expected) {
			return nil, fmt.Errorf("native helper installation changed the transport: %+v", actual)
		}
		return (ExecRunner{}).RunStreaming(ctx, Command{
			Name: os.Args[0],
			Args: []string{"-test.run=^TestNativeHelperPipedInputProcess$"},
			Env: []string{
				"PORTO_NATIVE_HELPER_PIPE_INPUT=1",
				"PORTO_NATIVE_HELPER_PIPE_DIGEST=" + digest,
			},
			StdinReader: actual.StdinReader,
		}, emit)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	actual, err := InstallNativeHelper(ctx, runner, command, executable, func(name string) (string, error) {
		return "", fmt.Errorf("unexpected fallback lookup for %q", name)
	})
	if err != nil || actual != digest || !streamed {
		t.Fatalf("checksum-verified binary did not reach piped subprocess stdin: digest=%q streamed=%t error=%v", actual, streamed, err)
	}
}

func TestNativeHelperPipedInputProcess(t *testing.T) {
	if os.Getenv("PORTO_NATIVE_HELPER_PIPE_INPUT") != "1" {
		return
	}
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		fmt.Fprintln(os.Stderr, "native helper stdin is not a pipe")
		os.Exit(42)
	}
	compressed, err := gzip.NewReader(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "native helper stdin is not a gzip stream:", err)
		os.Exit(45)
	}
	data, readErr := io.ReadAll(compressed)
	if err := errors.Join(readErr, compressed.Close()); err != nil {
		fmt.Fprintln(os.Stderr, "native helper stdin decompression failed:", err)
		os.Exit(43)
	}
	checksum := sha256.Sum256(data)
	if hex.EncodeToString(checksum[:]) != os.Getenv("PORTO_NATIVE_HELPER_PIPE_DIGEST") {
		fmt.Fprintln(os.Stderr, "native helper binary stream changed")
		os.Exit(44)
	}
	os.Exit(0)
}

func TestInstallNativeHelperCompressesTransferWithoutChangingDigest(t *testing.T) {
	data := bytes.Repeat([]byte("synthetic binary payload\x00\xff"), 8192)
	executable := nativeHelperBundle(t, data)
	checksum := sha256.Sum256(data)
	digest := hex.EncodeToString(checksum[:])
	runner := nativeHelperStreamingRunner{run: func(_ context.Context, command Command, _ func(OutputChunk) error) ([]byte, error) {
		payload, err := io.ReadAll(command.StdinReader)
		if err != nil {
			return nil, err
		}
		if len(payload) >= len(data)/2 {
			return nil, fmt.Errorf("compressible helper transfer was not reduced: raw=%d transferred=%d", len(data), len(payload))
		}
		compressed, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		decoded, readErr := io.ReadAll(compressed)
		if err := errors.Join(readErr, compressed.Close()); err != nil {
			return nil, err
		}
		if !bytes.Equal(decoded, data) || command.Args[len(command.Args)-1] != digest {
			return nil, errors.New("compression changed the helper binary or its uncompressed checksum")
		}
		return nil, nil
	}}
	actual, err := InstallNativeHelper(context.Background(), runner, Command{Name: "synthetic-transport"}, executable, func(string) (string, error) {
		return "", errors.New("unexpected fallback lookup")
	})
	if err != nil || actual != digest {
		t.Fatalf("helper transfer did not preserve the compressed payload contract: digest=%q error=%v", actual, err)
	}
}

func TestInstallNativeHelperPreservesCancellationAndBoundedDiagnostics(t *testing.T) {
	executable := nativeHelperBundle(t, []byte("synthetic helper"))
	for _, mode := range []string{"canceled", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if mode == "deadline" {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			} else {
				cancel()
			}
			defer cancel()
			transportError := errors.New("synthetic transport exited")
			runner := nativeHelperStreamingRunner{run: func(_ context.Context, _ Command, emit func(OutputChunk) error) ([]byte, error) {
				if err := emit(OutputChunk{Stream: "stderr", Data: []byte(strings.Repeat("d", 8192) + "discarded diagnostic tail")}); err != nil {
					return nil, err
				}
				return nil, transportError
			}}
			digest, err := InstallNativeHelper(ctx, runner, Command{Name: "synthetic-transport"}, executable, func(string) (string, error) {
				return "", errors.New("unexpected fallback lookup")
			})
			if digest != "" || !errors.Is(err, ctx.Err()) || !errors.Is(err, transportError) {
				t.Fatalf("helper failure lost its cause: digest=%q cancellation=%t transport=%t",
					digest, errors.Is(err, ctx.Err()), errors.Is(err, transportError))
			}
			if len(err.Error()) > 4608 || !strings.Contains(err.Error(), strings.Repeat("d", 4096)) ||
				strings.Contains(err.Error(), "discarded diagnostic tail") {
				t.Fatalf("helper diagnostics were lost or exceeded their bound: length=%d", len(err.Error()))
			}
		})
	}
}
