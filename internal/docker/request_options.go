package docker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
)

func decodeDockerOptionsJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	var raw json.RawMessage
	if !decodeDockerJSON(w, r, &raw) {
		return false
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid Docker request: expected an object"})
		return false
	}
	if err := json.Unmarshal(raw, value); err != nil {
		writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid Docker request: " + err.Error()})
		return false
	}
	unsupported, err := unsupportedDockerJSONOptions(raw, reflect.TypeOf(value), "", dockerRequestVersion(r))
	if err != nil {
		writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return false
	}
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		writeDockerUnsupported(w, "request options: "+strings.Join(unsupported, ", "))
		return false
	}
	return true
}

func unsupportedDockerJSONOptions(raw json.RawMessage, target reflect.Type, path, version string) ([]string, error) {
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var unsupported []string
	switch target.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
		for name, field := range fields {
			fieldPath := name
			if path != "" {
				fieldPath = path + "." + name
			}
			index := -1
			for candidate := range target.NumField() {
				member := target.Field(candidate)
				tag := strings.Split(member.Tag.Get("json"), ",")[0]
				if tag == "" {
					tag = member.Name
				}
				if tag != "-" && strings.EqualFold(tag, name) {
					index = candidate
					break
				}
			}
			if index < 0 {
				if !dockerJSONDefault(field) {
					unsupported = append(unsupported, fieldPath)
				}
				continue
			}
			if (strings.EqualFold(fieldPath, "HostConfig.MaskedPaths") ||
				strings.EqualFold(fieldPath, "HostConfig.ReadonlyPaths")) &&
				!bytes.Equal(bytes.TrimSpace(field), []byte("null")) {
				unsupported = append(unsupported, fieldPath)
				continue
			}
			if !dockerJSONDefault(field) {
				minimum := dockerOptionMinimumVersion(fieldPath)
				if minimum != "" && dockerVersionBefore(version, minimum) {
					return nil, fmt.Errorf("Docker option %s requires API %s or newer", fieldPath, minimum)
				}
			}
			nested, err := unsupportedDockerJSONOptions(field, target.Field(index).Type, fieldPath, version)
			if err != nil {
				return nil, err
			}
			unsupported = append(unsupported, nested...)
		}
	case reflect.Slice, reflect.Array:
		if target.Elem().Kind() != reflect.Struct && target.Elem().Kind() != reflect.Pointer {
			return nil, nil
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, err
		}
		for index, entry := range entries {
			nested, err := unsupportedDockerJSONOptions(entry, target.Elem(), fmt.Sprintf("%s[%d]", path, index), version)
			if err != nil {
				return nil, err
			}
			unsupported = append(unsupported, nested...)
		}
	case reflect.Map:
		if target.Elem().Kind() != reflect.Struct && target.Elem().Kind() != reflect.Pointer {
			return nil, nil
		}
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, err
		}
		for key, entry := range entries {
			nested, err := unsupportedDockerJSONOptions(entry, target.Elem(), path+"."+key, version)
			if err != nil {
				return nil, err
			}
			unsupported = append(unsupported, nested...)
		}
	}
	return unsupported, nil
}

func dockerJSONDefault(raw json.RawMessage) bool {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	switch value := value.(type) {
	case nil:
		return true
	case bool:
		return !value
	case string:
		return value == ""
	case json.Number:
		mantissa := strings.FieldsFunc(value.String(), func(character rune) bool {
			return character == 'e' || character == 'E'
		})[0]
		return strings.Trim(mantissa, "+-.0") == ""
	case []any:
		return len(value) == 0
	case map[string]any:
		return len(value) == 0
	default:
		return false
	}
}

func dockerOptionMinimumVersion(path string) string {
	for suffix, version := range map[string]string{
		".Healthcheck.StartInterval":          "1.44",
		".BindOptions.ReadOnlyNonRecursive":   "1.44",
		".BindOptions.ReadOnlyForceRecursive": "1.44",
		".VolumeOptions.Subpath":              "1.45",
		".TmpfsOptions.Options":               "1.46",
	} {
		if strings.HasSuffix("."+strings.ToLower(path), strings.ToLower(suffix)) {
			return version
		}
	}
	return ""
}
