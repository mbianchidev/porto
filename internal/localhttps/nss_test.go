package localhttps

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/certificates"
)

func TestChromiumTrustOwnsOnlyItsFingerprintNickname(t *testing.T) {
	root := t.TempDir()
	authority := filepath.Join(root, "ca.pem")
	if _, err := certificates.New(filepath.Join(root, "leaf.pem"), filepath.Join(root, "leaf.key"), authority, authority+".key").Ensure(); err != nil {
		t.Fatal(err)
	}
	data, fingerprint, err := readAuthority(authority)
	if err != nil {
		t.Fatal(err)
	}
	foreign := "Unrelated synthetic certificate"
	certificates := map[string][]byte{foreign: []byte("synthetic unrelated public certificate")}
	store := nssTrust{directory: filepath.Join(root, "nssdb")}
	store.run = func(args ...string) ([]byte, error) {
		argument := func(key string) string {
			for index, value := range args {
				if value == key && index+1 < len(args) {
					return args[index+1]
				}
			}
			return ""
		}
		nickname := argument("-n")
		switch {
		case args[0] == "-N":
			return nil, os.WriteFile(filepath.Join(store.directory, "cert9.db"), nil, 0o600)
		case args[0] == "-A":
			if argument("-t") != "C,," {
				return nil, errors.New("CA trust was broader than SSL")
			}
			certificates[nickname] = data
			return nil, nil
		case args[0] == "-L":
			if data, found := certificates[nickname]; found {
				return data, nil
			}
			return []byte("SEC_ERROR_UNKNOWN_CERT"), errors.New("not found")
		case args[0] == "-V":
			return nil, nil
		case args[0] == "-D":
			delete(certificates, nickname)
			return nil, nil
		default:
			return nil, errors.New("unexpected NSS operation")
		}
	}
	if err := store.set(authority, false); err != nil {
		t.Fatal(err)
	}
	if _, found := certificates["Porto Development CA "+fingerprint]; !found {
		t.Fatal("CA was not indexed by its exact fingerprint")
	}
	trusted, err := store.trusted(authority)
	if err != nil || !trusted {
		t.Fatalf("Chromium trust = %t, %v", trusted, err)
	}
	if err := store.set(authority, true); err != nil {
		t.Fatal(err)
	}
	if len(certificates) != 1 || !strings.Contains(string(certificates[foreign]), "unrelated") {
		t.Fatal("untrust removed an unrelated certificate")
	}
}
