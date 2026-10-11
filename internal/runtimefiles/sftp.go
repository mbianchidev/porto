package runtimefiles

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"

	"github.com/mbianchidev/porto/internal/datafiles"
)

func RunSFTP(ctx context.Context, encoded string, input io.Reader, output io.Writer) error {
	if len(encoded) == 0 || len(encoded) > 1024*1024 {
		return datafiles.ErrLimit
	}
	header, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return errors.Join(datafiles.ErrInvalid, err)
	}
	header = append(header, '\n')
	stop := context.AfterFunc(ctx, func() {
		if closer, ok := input.(io.Closer); ok {
			_ = closer.Close()
		}
	})
	defer stop()
	return Run(ctx, io.MultiReader(bytes.NewReader(header), input), output)
}

type sftpStdio struct {
	io.Reader
	io.Writer
}

func (s *sftpStdio) Close() error {
	if closer, ok := s.Writer.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}
