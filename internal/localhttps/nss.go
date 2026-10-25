package localhttps

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type nssTrust struct {
	directory string
	run       func(...string) ([]byte, error)
}

func (s nssTrust) initialized() (bool, error) {
	if info, err := os.Lstat(s.directory); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false, errors.New("Chromium NSS trust directory must be a regular user-owned directory")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else {
		return false, err
	}
	if _, err := os.Stat(filepath.Join(s.directory, "cert9.db")); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	for _, name := range []string{"cert8.db", "key3.db", "key4.db", "pkcs11.txt"} {
		if _, err := os.Stat(filepath.Join(s.directory, name)); err == nil {
			return false, errors.New("existing Chromium NSS database needs browser migration or repair; Porto will not reset it")
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func (s nssTrust) lookup(authorityPath string) (string, bool, error) {
	authority, fingerprint, err := readAuthorityIdentity(authorityPath)
	if err != nil {
		return "", false, err
	}
	nickname := "Porto Development CA " + fingerprint
	initialized, err := s.initialized()
	if err != nil || !initialized {
		return nickname, false, err
	}
	data, found, err := s.certificate(nickname)
	if err != nil || !found {
		return nickname, false, err
	}
	expected, _ := pem.Decode(authority)
	if !bytes.Equal(expected.Bytes, data) {
		return nickname, false, errors.New("Chromium CA nickname contains a different certificate; refusing to replace or remove it")
	}
	return nickname, true, nil
}

func (s nssTrust) certificate(nickname string) (data []byte, found bool, err error) {
	// certutil -L -n exports every same-subject certificate, in arbitrary order.
	// Read the exact PKCS#11 CKA_LABEL/CKA_VALUE in the supported SQL store.
	databasePath := filepath.ToSlash(filepath.Join(s.directory, "cert9.db"))
	if !strings.HasPrefix(databasePath, "/") {
		databasePath = "/" + databasePath
	}
	location := url.URL{Scheme: "file", Path: databasePath, RawQuery: "mode=ro"}
	database, err := sql.Open("sqlite", location.String())
	if err != nil {
		return nil, false, fmt.Errorf("open Chromium public certificate identities: %w", err)
	}
	defer func() { err = errors.Join(err, database.Close()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	err = database.QueryRowContext(ctx,
		`SELECT a11, COUNT(*) OVER () FROM nssPublic WHERE a0 = x'00000001' AND a3 = ?`,
		[]byte(nickname)).Scan(&data, &count)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect Chromium certificate nickname identity: %w", err)
	}
	if count != 1 {
		return nil, false, errors.New("Chromium CA nickname is ambiguous; refusing to change its certificates")
	}
	return data, true, nil
}

func (s nssTrust) trusted(authorityPath string) (bool, error) {
	nickname, found, err := s.lookup(authorityPath)
	if err != nil || !found {
		return false, err
	}
	if output, err := s.run("-V", "-d", "sql:"+s.directory, "-n", nickname, "-u", "L"); err != nil {
		return false, fmt.Errorf("Chromium SSL CA trust is unavailable: %w: %s", err, output)
	}
	return true, nil
}

func (s nssTrust) set(authorityPath string, remove bool) error {
	if !remove {
		if _, _, err := readAuthority(authorityPath); err != nil {
			return err
		}
	}
	nickname, found, err := s.lookup(authorityPath)
	if err != nil {
		return err
	}
	if remove {
		if !found {
			return nil
		}
		if output, err := s.run("-D", "-d", "sql:"+s.directory, "-n", nickname); err != nil {
			return fmt.Errorf("remove only Porto's Chromium CA: %w: %s", err, output)
		}
		return nil
	}
	if initialized, err := s.initialized(); err != nil {
		return err
	} else if !initialized {
		if err := os.MkdirAll(s.directory, 0o700); err != nil {
			return err
		}
		if output, err := s.run("-N", "-d", "sql:"+s.directory, "--empty-password"); err != nil {
			return fmt.Errorf("initialize an empty user Chromium trust database: %w: %s", err, output)
		}
	}
	action := "-A"
	if found {
		action = "-M"
	}
	arguments := []string{action, "-d", "sql:" + s.directory, "-n", nickname, "-t", "C,,"}
	if !found {
		arguments = append(arguments, "-i", authorityPath)
	}
	if output, err := s.run(arguments...); err != nil {
		return fmt.Errorf("trust Porto's CA for Chromium SSL only: %w: %s", err, output)
	}
	return nil
}
