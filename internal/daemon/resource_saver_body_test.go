package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/resourcesaver"
)

func TestQueuedBodyStagingPreservesBinaryRequestsAndRemovesPrivateFiles(t *testing.T) {
	payload := bytes.Repeat([]byte("synthetic\x00binary\xff"), 8192)
	for _, chunked := range []bool{false, true} {
		t.Run(fmt.Sprintf("chunked=%t", chunked), func(t *testing.T) {
			removed := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
				defer cancel()
				body, err := stageQueuedRequestBody(ctx, w, request, int64(len(payload)))
				if err != nil || body == nil {
					http.Error(w, fmt.Sprintf("stage initiating upload: %v", err), http.StatusInternalServerError)
					return
				}
				defer func() {
					if err := body.Close(); err != nil {
						t.Error(err)
					}
					removed <- body.Name()
				}()
				info, err := body.Stat()
				if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
					http.Error(w, "queued body was not a private file", http.StatusInternalServerError)
					return
				}
				data, err := io.ReadAll(request.Body)
				if err != nil || request.Method != http.MethodPut || request.Header.Get("X-Synthetic") != "unchanged" {
					http.Error(w, "initiating request metadata was changed", http.StatusInternalServerError)
					return
				}
				fmt.Fprintf(w, "%x", sha256.Sum256(data))
			}))
			defer server.Close()
			var input io.Reader = bytes.NewReader(payload)
			if chunked {
				input = io.NopCloser(input)
			}
			request, err := http.NewRequest(http.MethodPut, server.URL, input)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("X-Synthetic", "unchanged")
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			data, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if response.StatusCode != http.StatusOK || readErr != nil || closeErr != nil || string(data) != fmt.Sprintf("%x", sha256.Sum256(payload)) {
				t.Fatalf("initiating body was not retained exactly: %s: %s: %v: %v", response.Status, data, readErr, closeErr)
			}
			if _, err := os.Stat(<-removed); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("queued-body file was not removed: %v", err)
			}
		})
	}
}

func TestChunkedQueuedBodyOverrunIsRejectedAndDoesNotLeakItsFile(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"TMPDIR", "TEMP", "TMP"} {
		t.Setenv(name, directory)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		ctx, cancel := context.WithTimeout(request.Context(), time.Second)
		defer cancel()
		body, err := stageQueuedRequestBody(ctx, w, request, 4)
		if body != nil || !errors.Is(err, resourcesaver.ErrQueuedBodyTooLarge) {
			http.Error(w, "oversized queued body was accepted", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}))
	defer server.Close()
	response, err := server.Client().Post(server.URL, "application/octet-stream", io.NopCloser(strings.NewReader("synthetic oversized body")))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked queued-body overrun = %s", response.Status)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed queued body leaked temporary data: %v, %v", entries, err)
	}
}
