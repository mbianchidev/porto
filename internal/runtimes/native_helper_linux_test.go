package runtimes

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativeHelperGuestInstallationPreservesVerifiedPublishAndFailureCleanup(t *testing.T) {
	for _, mode := range []string{"valid", "corrupt gzip", "checksum mismatch", "version failure"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			data := []byte("#!/bin/sh\nset -eu\n[ \"$1\" = version ]\nprintf 'synthetic version\\n' > \"$HOME/verified-version\"\n")
			if mode == "version failure" {
				data = []byte("#!/bin/sh\nexit 19\n")
			}
			executable := nativeHelperBundle(t, data)
			checksum := sha256.Sum256(data)
			digest := hex.EncodeToString(checksum[:])
			directory := filepath.Join(home, ".local", "share", "porto", "native-helpers", digest)
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			installed := filepath.Join(directory, "porto-runtime-helper")
			original := []byte("previous synthetic helper")
			if err := os.WriteFile(installed, original, 0o600); err != nil {
				t.Fatal(err)
			}
			runner := nativeHelperStreamingRunner{run: func(ctx context.Context, command Command, emit func(OutputChunk) error) ([]byte, error) {
				payload, err := io.ReadAll(command.StdinReader)
				if err != nil {
					return nil, err
				}
				switch mode {
				case "corrupt gzip":
					payload[len(payload)-1] ^= 0xff
				case "checksum mismatch":
					var compressed bytes.Buffer
					writer := gzip.NewWriter(&compressed)
					_, writeErr := writer.Write([]byte("different synthetic helper"))
					if err := errors.Join(writeErr, writer.Close()); err != nil {
						return nil, err
					}
					payload = compressed.Bytes()
				}
				command.StdinReader = bytes.NewReader(payload)
				return (ExecRunner{}).RunStreaming(ctx, command, emit)
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			actual, err := InstallNativeHelper(ctx, runner, Command{
				Name: "env", Env: []string{"HOME=" + home},
			}, executable, func(string) (string, error) {
				return "", errors.New("unexpected fallback lookup")
			})
			if mode == "valid" {
				if err != nil || actual != digest {
					t.Fatalf("verified guest installation failed: digest=%q error=%v", actual, err)
				}
				contents, err := os.ReadFile(installed)
				if err != nil || !bytes.Equal(contents, data) {
					t.Fatalf("published helper changed: %q, %v", contents, err)
				}
				info, err := os.Stat(installed)
				if err != nil || info.Mode().Perm() != 0o755 {
					t.Fatalf("published helper is not executable: %v, %v", info, err)
				}
				if contents, err := os.ReadFile(filepath.Join(home, "verified-version")); err != nil || string(contents) != "synthetic version\n" {
					t.Fatalf("helper version was not executed before publication: %q, %v", contents, err)
				}
			} else {
				if err == nil || actual != "" || !strings.Contains(err.Error(), "Receiving compressed Porto native helper") {
					t.Fatalf("failed guest installation was hidden or lost stage diagnostics: digest=%q error=%v", actual, err)
				}
				contents, readErr := os.ReadFile(installed)
				if readErr != nil || !bytes.Equal(contents, original) {
					t.Fatalf("failed installation replaced the existing helper: %q, %v", contents, readErr)
				}
				if _, err := os.Stat(filepath.Join(home, "verified-version")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("an invalid helper was executed: %v", err)
				}
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 1 || entries[0].Name() != "porto-runtime-helper" {
				t.Fatalf("guest installation left temporary files: %+v, %v", entries, err)
			}
		})
	}
}
