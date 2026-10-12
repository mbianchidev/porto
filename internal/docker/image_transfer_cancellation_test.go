package docker

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type contextImageRunner struct {
	*fakeRunner
	stream func(context.Context, runtimes.Command, func(runtimes.OutputChunk) error) ([]byte, error)
}

func (runner contextImageRunner) RunStreaming(ctx context.Context, command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
	return runner.stream(ctx, command, emit)
}

func TestImageTransferCancelsBackendWhenProgressTransportFails(t *testing.T) {
	transportError := errors.New("synthetic closed progress transport")
	runner := contextImageRunner{
		fakeRunner: &fakeRunner{},
		stream: func(ctx context.Context, command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
			if err := emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte("synthetic progress")}); !errors.Is(err, transportError) {
				t.Fatalf("progress transport error = %v", err)
			}
			select {
			case <-ctx.Done():
				return nil, context.Cause(ctx)
			case <-time.After(time.Second):
				t.Fatal("backend was not canceled when the progress transport failed")
				return nil, nil
			}
		},
	}
	manager := New(runner)
	err := manager.StreamLoadImages(context.Background(), strings.NewReader("synthetic archive"), false,
		func(runtimes.OutputChunk) error { return transportError })
	if !errors.Is(err, transportError) {
		t.Fatalf("transfer transport error was hidden: %v", err)
	}
}

func TestImagePushCleansTemporaryAuthAfterCancellationAndPartialFailure(t *testing.T) {
	for _, cancelTransfer := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial registry failure", true: "client disconnect"}[cancelTransfer], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var authDirectory string
			runner := contextImageRunner{
				fakeRunner: &fakeRunner{},
				stream: func(ctx context.Context, command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
					for _, entry := range command.Env {
						if value, ok := strings.CutPrefix(entry, "DOCKER_CONFIG="); ok {
							authDirectory = value
						}
					}
					if _, err := os.Stat(filepath.Join(authDirectory, "config.json")); err != nil {
						t.Fatal(err)
					}
					if err := emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte("synthetic layer uploaded")}); err != nil {
						return nil, err
					}
					if cancelTransfer {
						cancel()
						<-ctx.Done()
						return nil, context.Cause(ctx)
					}
					return nil, errors.New("synthetic registry rejected manifest digest")
				},
			}
			err := New(runner).StreamPushImageWithAuth(ctx, "registry.example.test/synthetic:v1", "",
				&RegistryAuth{Username: "synthetic", Password: "synthetic-token", ServerAddress: "registry.example.test"},
				func(runtimes.OutputChunk) error { return nil })
			if err == nil {
				t.Fatal("failed image push was reported as successful")
			}
			if cancelTransfer && !errors.Is(err, context.Canceled) {
				t.Fatalf("client disconnect was hidden: %v", err)
			}
			if authDirectory == "" {
				t.Fatal("authenticated push did not scope its credentials")
			}
			if _, err := os.Stat(authDirectory); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temporary registry credentials leaked: %v", err)
			}
		})
	}
}

func TestImageImportPropagatesInterruptedUploadWithoutSuccess(t *testing.T) {
	uploadError := errors.New("synthetic archive upload interrupted")
	runner := contextImageRunner{
		fakeRunner: &fakeRunner{},
		stream: func(_ context.Context, command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
			_, err := io.Copy(io.Discard, command.StdinReader)
			return nil, err
		},
	}
	err := New(runner).StreamImportImage(context.Background(), interruptedImageArchive{uploadError}, "synthetic:v1", "", "",
		func(runtimes.OutputChunk) error { return nil })
	if !errors.Is(err, uploadError) {
		t.Fatalf("interrupted upload error = %v", err)
	}
}

type interruptedImageArchive struct{ err error }

func (reader interruptedImageArchive) Read([]byte) (int, error) { return 0, reader.err }
