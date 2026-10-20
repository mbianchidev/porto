//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

func runPTYCommand(ctx context.Context, args []string, input io.Reader, output io.Writer) error {
	flags := flag.NewFlagSet("pty-command", flag.ContinueOnError)
	height := flags.Uint("height", 24, "terminal rows")
	width := flags.Uint("width", 80, "terminal columns")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() == 0 || *height > 65535 || *width > 65535 {
		return errors.New("invalid terminal dimensions or missing terminal command")
	}
	if *height == 0 {
		*height = 24
	}
	if *width == 0 {
		*width = 80
	}
	command := exec.CommandContext(ctx, flags.Arg(0), flags.Args()[1:]...)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: uint16(*height), Cols: uint16(*width)})
	if err != nil {
		return fmt.Errorf("start terminal command: %w", err)
	}
	defer terminal.Close()
	go func() {
		_, _ = io.Copy(terminal, input)
		_, _ = terminal.Write([]byte{4})
	}()
	_, copyErr := io.Copy(output, terminal)
	if copyErr != nil && !errors.Is(copyErr, syscall.EIO) {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	if errors.Is(copyErr, syscall.EIO) {
		copyErr = nil
	}
	if copyErr != nil {
		return errors.Join(copyErr, waitErr)
	}
	return waitErr
}
