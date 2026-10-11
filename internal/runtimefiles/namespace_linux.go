//go:build linux

package runtimefiles

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"

	"github.com/mbianchidev/porto/internal/datafiles"
)

func dispatchRuntimeNamespace(ctx context.Context, descriptor Descriptor, request datafiles.Request, input io.Reader, output io.Writer) (bool, error) {
	if descriptor.RuntimePID <= 0 || (descriptor.Resource.Kind != "container" && descriptor.Resource.Kind != "image") {
		return false, nil
	}
	if descriptor.Resource.Kind == "container" && descriptor.PID > 0 && request.Action != "attach" && request.Action != "detach" {
		return false, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return true, err
	}
	descriptor.NamespacePID = descriptor.RuntimePID
	header, err := json.Marshal(Envelope{Descriptor: descriptor, Request: request})
	if err != nil {
		return true, err
	}
	header = append(header, '\n')
	command := exec.CommandContext(ctx, "nsenter", "--target", strconv.FormatInt(int64(descriptor.RuntimePID), 10), "--mount", "--user", "--", executable, "files")
	command.Stdin = io.MultiReader(bytes.NewReader(header), input)
	command.Stdout = output
	var diagnostics boundedNamespaceDiagnostics
	command.Stderr = &diagnostics
	if err := command.Run(); err != nil {
		return true, errors.Join(ctx.Err(), fmt.Errorf("enter owned containerd filesystem namespace: %w: %s", err, diagnostics.String()))
	}
	return true, nil
}

type boundedNamespaceDiagnostics struct{ bytes.Buffer }

func (b *boundedNamespaceDiagnostics) Write(data []byte) (int, error) {
	length := len(data)
	remaining := 4096 - b.Len()
	if remaining > 0 {
		_, _ = b.Buffer.Write(data[:min(len(data), remaining)])
	}
	return length, nil
}
