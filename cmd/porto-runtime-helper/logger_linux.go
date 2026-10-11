//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"io"

	"github.com/containerd/containerd/v2/core/runtime/v2/logging"
	"github.com/mbianchidev/porto/internal/containerlogs"
)

func runShimLogger(args []string) error {
	flags := flag.NewFlagSet("container-logger", flag.ContinueOnError)
	logPath := flags.String("porto-log", "", "Porto-owned log path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("invalid container logger arguments")
	}
	logging.Run(func(ctx context.Context, config *logging.Config, ready func() error) (err error) {
		recorder, closeRecorder, err := containerlogs.Open(*logPath)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, closeRecorder()) }()
		if err := ready(); err != nil {
			return err
		}
		done := make(chan error, 2)
		go func() { _, err := io.Copy(recorder.Writer("stdout"), config.Stdout); done <- err }()
		go func() { _, err := io.Copy(recorder.Writer("stderr"), config.Stderr); done <- err }()
		var result error
		for remaining := 2; remaining > 0; remaining-- {
			select {
			case streamErr := <-done:
				result = errors.Join(result, streamErr)
			case <-ctx.Done():
				if closer, ok := config.Stdout.(io.Closer); ok {
					_ = closer.Close()
				}
				if closer, ok := config.Stderr.(io.Closer); ok {
					_ = closer.Close()
				}
				for range remaining {
					result = errors.Join(result, <-done)
				}
				return result
			}
		}
		return result
	})
	return nil
}
