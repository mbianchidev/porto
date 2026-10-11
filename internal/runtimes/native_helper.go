package runtimes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func BundledRuntimeHelper(executable string, lookPath func(string) (string, error)) (string, error) {
	if executable != "" {
		bundled := filepath.Join(filepath.Dir(executable), "runtime", "bin", "porto-runtime-helper")
		info, err := os.Stat(bundled)
		if err == nil {
			if !info.Mode().IsRegular() {
				return "", errors.New("bundled Porto runtime helper is not a regular file")
			}
			return bundled, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	name, err := lookPath("porto-runtime-helper")
	if err != nil {
		return "", nil
	}
	return name, nil
}

const installNativeHelperScript = `set -eu
umask 077
digest="$1"
base="$HOME/.local/share/porto/native-helpers/$digest"
mkdir -p "$base"
temporary="$(mktemp "$base/.install.XXXXXX")"
trap 'rm -f "$temporary"' EXIT HUP INT TERM
cat > "$temporary"
actual="$(sha256sum "$temporary")"
actual="${actual%% *}"
if [ "$actual" != "$digest" ]; then
  echo 'Porto native helper integrity verification failed' >&2
  exit 1
fi
chmod 0755 "$temporary"
"$temporary" version
mv -f "$temporary" "$base/porto-runtime-helper"
`

func InstallNativeHelper(ctx context.Context, runner Runner, command Command, executable string, lookPath func(string) (string, error)) (string, error) {
	helper, err := BundledRuntimeHelper(executable, lookPath)
	if err != nil || helper == "" {
		return "", errors.Join(errors.New("Porto native filesystem helper is unavailable; install the full desktop package"), err)
	}
	file, err := os.Open(helper)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	streamer, ok := runner.(interface {
		RunStreaming(context.Context, Command, func(OutputChunk) error) ([]byte, error)
	})
	if !ok {
		return "", errors.New("native VM helper installation requires streaming input")
	}
	command.Args = append(command.Args, "sh", "-c", installNativeHelperScript, "porto-native-helper", digest)
	command.StdinReader = file
	var diagnostics strings.Builder
	_, err = streamer.RunStreaming(ctx, command, func(chunk OutputChunk) error {
		if remaining := 4096 - diagnostics.Len(); remaining > 0 {
			diagnostics.Write(chunk.Data[:min(len(chunk.Data), remaining)])
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("install checksum-verified native VM helper: %w: %s", err, diagnostics.String())
	}
	return digest, nil
}
