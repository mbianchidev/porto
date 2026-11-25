package nativefiles

import (
	"encoding/base64"
	"encoding/json"

	"github.com/porto-containers/porto/internal/datafiles"
	"github.com/porto-containers/porto/internal/runtimefiles"
)

func EncodeNativeRequest(descriptor runtimefiles.Descriptor, writable bool) (string, error) {
	document, err := json.Marshal(runtimefiles.Envelope{
		Descriptor: descriptor,
		Request:    datafiles.Request{Action: "sftp", Identity: descriptor.Resource.Fingerprint(), Writable: writable},
	})
	if err != nil {
		return "", err
	}
	if len(document) > 16*1024 {
		return "", datafiles.ErrLimit
	}
	return base64.RawURLEncoding.EncodeToString(document), nil
}
