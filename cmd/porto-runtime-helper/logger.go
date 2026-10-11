package main

import (
	"errors"
	"flag"
	"io"
	"os"

	"github.com/mbianchidev/porto/internal/containerlogs"
)

func logStdio(args []string) (err error) {
	flags := flag.NewFlagSet("log-stdio", flag.ContinueOnError)
	logPath := flags.String("path", "", "Porto-owned raw log path")
	stream := flags.String("stream", "", "stdout or stderr")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*stream != "stdout" && *stream != "stderr") {
		return errors.New("log-stdio requires --path and --stream=stdout|stderr")
	}
	recorder, closeRecorder, err := containerlogs.Open(*logPath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeRecorder()) }()
	_, err = io.Copy(io.MultiWriter(recorder.Writer(*stream), os.Stdout), os.Stdin)
	return err
}
