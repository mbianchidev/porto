package networking

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestUnavailableProxyStatusKeepsEditableCollections(t *testing.T) {
	manager := New(t.TempDir())
	manager.system = func() (Config, string, error) { return Config{}, "", errors.New("synthetic PAC unavailable") }
	if err := manager.Save(Config{Mode: "system"}); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Snapshot()
	if err == nil {
		t.Fatal("proxy failure was not reported")
	}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"registryCAs":[]`, `"credentialsStored":{}`} {
		if !strings.Contains(string(data), expected) {
			t.Fatalf("unavailable status lost editable collections %s: %s", expected, data)
		}
	}
}
