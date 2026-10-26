package docker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type imageUploadRecorder struct{ *httptest.ResponseRecorder }

func (imageUploadRecorder) EnableFullDuplex() error { return nil }

func newImageUploadRecorder() imageUploadRecorder {
	return imageUploadRecorder{httptest.NewRecorder()}
}

func TestDockerImageUploadsRejectUnsupportedDuplexBeforeMutation(t *testing.T) {
	for _, path := range []string{"/v1.47/images/load", "/v1.47/images/create?fromSrc=-&repo=synthetic"} {
		t.Run(path, func(t *testing.T) {
			runner := &fakeRunner{streamer: func(runtimes.Command, func(runtimes.OutputChunk) error) ([]byte, error) {
				t.Fatal("image mutation started without a safe upload transport")
				return nil, nil
			}}
			response := httptest.NewRecorder()
			NewAPI(New(runner), "/tmp/porto.sock").ServeHTTP(response,
				httptest.NewRequest(http.MethodPost, path, strings.NewReader("synthetic archive")))
			if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "full-duplex image upload") {
				t.Fatalf("unsupported upload transport = %d: %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestDockerImageUploadProgressPreservesUnreadHTTPBody(t *testing.T) {
	for _, path := range []string{"/v1.47/images/load", "/v1.47/images/create?fromSrc=-&repo=synthetic"} {
		for _, chunked := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/chunked=%t", path, chunked), func(t *testing.T) {
				archive := bytes.Repeat([]byte("synthetic\x00archive\xff\n"), 4096)
				received := make(chan []byte, 1)
				runner := &fakeRunner{streamer: func(command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
					if err := emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte("Importing\n")}); err != nil {
						return nil, err
					}
					data, err := io.ReadAll(command.StdinReader)
					received <- data
					if err != nil {
						return nil, err
					}
					return nil, emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte("Loaded\n")})
				}}
				server := httptest.NewServer(NewAPI(New(runner), "/tmp/porto.sock"))
				defer server.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				var body io.Reader = bytes.NewReader(archive)
				if chunked {
					body = io.NopCloser(body)
				}
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+path, body)
				if err != nil {
					t.Fatal(err)
				}
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				progress, err := io.ReadAll(response.Body)
				if err != nil || response.StatusCode != http.StatusOK || bytes.Contains(progress, []byte(`"error"`)) {
					t.Fatalf("upload response = %d %s, error=%v", response.StatusCode, progress, err)
				}
				select {
				case data := <-received:
					if !bytes.Equal(data, archive) {
						t.Fatalf("early progress consumed upload bytes: received %d, want %d", len(data), len(archive))
					}
				case <-ctx.Done():
					t.Fatal("upload did not finish after early progress")
				}
			})
		}
	}
}

func TestDockerAPIImageTag(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if strings.Join(command.Args, " ") != "tag alpine:latest example/app:v1" {
			return nil, errors.New("unexpected tag command: " + strings.Join(command.Args, " "))
		}
		return nil, nil
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1.47/images/alpine:latest/tag?repo=example%2Fapp&tag=v1",
		nil,
	)

	NewAPI(New(runner), "/tmp/porto.sock").ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("tag = %d: %s", response.Code, response.Body.String())
	}
}

func TestDockerAPIImageLoadStreamsArchiveAndProgress(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.streamer = func(command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
		if strings.Join(command.Args, " ") != "load" {
			return nil, errors.New("unexpected load command: " + strings.Join(command.Args, " "))
		}
		data, err := io.ReadAll(command.StdinReader)
		if err != nil {
			return nil, err
		}
		if string(data) != "synthetic-image-archive" {
			return nil, errors.New("image archive was not streamed to the runtime")
		}
		if err := emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte("unpacking example/app:v1\n")}); err != nil {
			return nil, err
		}
		return nil, nil
	}
	response := newImageUploadRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1.47/images/load",
		bytes.NewBufferString("synthetic-image-archive"),
	)

	NewAPI(New(runner), "/tmp/porto.sock").ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "unpacking example/app:v1") {
		t.Fatalf("load = %d: %s", response.Code, response.Body.String())
	}
	if !response.Flushed {
		t.Fatal("load progress was never flushed")
	}
}

func TestDockerAPIImagePushUsesRegistryAuthAndCleansTemporaryConfig(t *testing.T) {
	var configDirectory string
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.streamer = func(command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
		if strings.Join(command.Args, " ") != "push registry.example.com/team/app:v1" {
			return nil, errors.New("unexpected push command: " + strings.Join(command.Args, " "))
		}
		for _, entry := range command.Env {
			if value, ok := strings.CutPrefix(entry, "DOCKER_CONFIG="); ok {
				configDirectory = value
			}
		}
		if configDirectory == "" {
			return nil, errors.New("authenticated push did not set DOCKER_CONFIG")
		}
		data, err := os.ReadFile(filepath.Join(configDirectory, "config.json"))
		if err != nil {
			return nil, err
		}
		if !bytes.Contains(data, []byte("registry.example.com")) ||
			!bytes.Contains(data, []byte(base64.StdEncoding.EncodeToString([]byte("user:token")))) {
			return nil, errors.New("registry credentials missing from temporary config")
		}
		if err := emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte("pushed manifest sha256:abc\n")}); err != nil {
			return nil, err
		}
		return nil, nil
	}
	auth, err := json.Marshal(map[string]string{
		"username":      "user",
		"password":      "token",
		"serveraddress": "registry.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1.47/images/registry.example.com/team/app/push?tag=v1",
		nil,
	)
	request.Header.Set("X-Registry-Auth", base64.RawURLEncoding.EncodeToString(auth))

	NewAPI(New(runner), "/tmp/porto.sock").ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "pushed manifest") {
		t.Fatalf("push = %d: %s", response.Code, response.Body.String())
	}
	if !response.Flushed {
		t.Fatal("push progress was never flushed")
	}
	if configDirectory == "" {
		t.Fatal("temporary registry config path was not observed")
	}
	if _, err := os.Stat(configDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary registry config was not removed: %v", err)
	}
}

func TestDockerAPIImageLoadReportsMalformedArchiveFailure(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.streamer = func(runtimes.Command, func(runtimes.OutputChunk) error) ([]byte, error) {
		return []byte("invalid tar header"), errors.New("exit status 1")
	}
	response := newImageUploadRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1.47/images/load", bytes.NewBufferString("broken"))

	NewAPI(New(runner), "/tmp/porto.sock").ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "invalid tar header") {
		t.Fatalf("load failure = %d: %s", response.Code, response.Body.String())
	}
}

func TestDockerAPIImageImportStreamsRootFilesystem(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.streamer = func(command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
		if strings.Join(command.Args, " ") != "image import --platform linux/amd64 --message seed - example/rootfs:v1" {
			return nil, errors.New("unexpected import command: " + strings.Join(command.Args, " "))
		}
		data, err := io.ReadAll(command.StdinReader)
		if err != nil || string(data) != "synthetic-rootfs" {
			return nil, errors.New("rootfs archive was not streamed")
		}
		return nil, emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte("sha256:imported\n")})
	}
	response := newImageUploadRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1.47/images/create?fromSrc=-&repo=example%2Frootfs&tag=v1&platform=linux%2Famd64&message=seed",
		bytes.NewBufferString("synthetic-rootfs"),
	)

	NewAPI(New(runner), "/tmp/porto.sock").ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "sha256:imported") {
		t.Fatalf("import = %d: %s", response.Code, response.Body.String())
	}
	if !response.Flushed {
		t.Fatal("import progress was never flushed")
	}
}

func TestDockerImageTransfersFlushBeforeBackendCompletion(t *testing.T) {
	for _, path := range []string{
		"/v1.47/images/load",
		"/v1.47/images/create?fromSrc=-&repo=synthetic",
		"/v1.47/images/registry.example.test/synthetic/push",
	} {
		t.Run(path, func(t *testing.T) {
			response := newImageUploadRecorder()
			runner := &fakeRunner{streamer: func(_ runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
				if err := emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte("synthetic transfer in progress\n")}); err != nil {
					return nil, err
				}
				if !response.Flushed {
					return nil, errors.New("progress did not reach the client before backend completion")
				}
				return nil, errors.New("synthetic partial digest failure")
			}}
			NewAPI(New(runner), "/tmp/porto.sock").ServeHTTP(response,
				httptest.NewRequest(http.MethodPost, path, strings.NewReader("synthetic archive")))
			var frames []map[string]any
			decoder := json.NewDecoder(response.Body)
			for decoder.More() {
				var frame map[string]any
				if err := decoder.Decode(&frame); err != nil {
					t.Fatal(err)
				}
				frames = append(frames, frame)
			}
			if len(frames) != 2 || !strings.Contains(fmt.Sprint(frames[1]["error"]), "synthetic partial digest failure") {
				t.Fatalf("progress/error frames = %+v", frames)
			}
		})
	}
}
