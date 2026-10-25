package localhttps

import (
	"database/sql"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	var database *sql.DB
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
			database = testNSSDatabase(t, store.directory)
			_, err := database.Exec(`INSERT INTO nssPublic (a0, a3, a11) VALUES (x'00000001', ?, ?)`,
				[]byte(foreign), certificates[foreign])
			return nil, err
		case args[0] == "-A":
			if argument("-t") != "C,," {
				return nil, errors.New("CA trust was broader than SSL")
			}
			certificates[nickname] = data
			block, _ := pem.Decode(data)
			_, err := database.Exec(`INSERT INTO nssPublic (a0, a3, a11) VALUES (x'00000001', ?, ?)`,
				[]byte(nickname), block.Bytes)
			return nil, err
		case args[0] == "-V":
			return nil, nil
		case args[0] == "-D":
			delete(certificates, nickname)
			_, err := database.Exec(`DELETE FROM nssPublic WHERE a3 = ?`, []byte(nickname))
			return nil, err
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

func testNSSDatabase(t *testing.T, directory string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", filepath.Join(directory, "cert9.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := database.Exec(`CREATE TABLE nssPublic (a0 BLOB, a3 BLOB, a11 BLOB)`); err != nil {
		t.Fatal(err)
	}
	return database
}

func TestChromiumTrustRefusesForeignNicknameEvenWhenAuthorityIsElsewhere(t *testing.T) {
	root := t.TempDir()
	authority := filepath.Join(root, "ca.pem")
	if _, err := certificates.New(filepath.Join(root, "leaf.pem"), filepath.Join(root, "leaf.key"), authority, authority+".key").Ensure(); err != nil {
		t.Fatal(err)
	}
	data, fingerprint, err := readAuthority(authority)
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := pem.Decode(data)
	store := nssTrust{directory: filepath.Join(root, "nssdb"), run: func(...string) ([]byte, error) {
		t.Fatal("ambiguous ownership mutated NSS trust")
		return nil, nil
	}}
	database := testNSSDatabase(t, store.directory)
	nickname := "Porto Development CA " + fingerprint
	for _, certificate := range []struct {
		nickname string
		der      []byte
	}{
		{nickname, []byte("synthetic foreign certificate")},
		{"another nickname", expected.Bytes},
	} {
		if _, err := database.Exec(`INSERT INTO nssPublic (a0, a3, a11) VALUES (x'00000001', ?, ?)`,
			[]byte(certificate.nickname), certificate.der); err != nil {
			t.Fatal(err)
		}
	}
	for _, remove := range []bool{false, true} {
		if err := store.set(authority, remove); err == nil {
			t.Fatal("a different certificate's nickname was adopted")
		}
	}
}

func TestChromiumTrustPreservesRealSameSubjectAuthorities(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses its native certificate store, not NSS")
	}
	binary, err := exec.LookPath("certutil")
	if err != nil {
		t.Skip("NSS certutil is not installed; real Linux browser trust remains a mandatory native CI gate")
	}
	root := t.TempDir()
	store := nssTrust{directory: filepath.Join(root, "nssdb"), run: func(args ...string) ([]byte, error) {
		return exec.Command(binary, args...).CombinedOutput()
	}}
	var authorities []string
	for _, name := range []string{"first", "second"} {
		authority := filepath.Join(root, name+"-ca.pem")
		if _, err := certificates.New(filepath.Join(root, name+".pem"), filepath.Join(root, name+".key"),
			authority, authority+".key").Ensure(); err != nil {
			t.Fatal(err)
		}
		if err := store.set(authority, false); err != nil {
			t.Fatal(err)
		}
		authorities = append(authorities, authority)
	}
	for _, authority := range authorities {
		if trusted, err := store.trusted(authority); err != nil || !trusted {
			t.Fatalf("same-subject CA trust = %t, %v", trusted, err)
		}
	}
	if err := store.set(authorities[0], true); err != nil {
		t.Fatal(err)
	}
	if trusted, err := store.trusted(authorities[1]); err != nil || !trusted {
		t.Fatalf("untrust removed the unrelated same-subject CA: %t, %v", trusted, err)
	}
}
