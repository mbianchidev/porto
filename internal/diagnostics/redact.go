package diagnostics

import (
	"bytes"
	"regexp"
	"strings"
)

const redactedValue = "[REDACTED]"

type RedactionContext struct {
	HomeDirectory string
	Hostname      string
	Username      string
}

var (
	pemSecretPattern    = regexp.MustCompile(`(?s)-----BEGIN (?:[A-Z0-9 ]*PRIVATE KEY|CERTIFICATE)-----.*?-----END (?:[A-Z0-9 ]*PRIVATE KEY|CERTIFICATE)-----`)
	jsonSecretPattern   = regexp.MustCompile(`(?i)("(?:[^"]*(?:password|passwd|token|secret|authorization|auth|key-data|certificate-data|proxy)[^"]*)"\s*:\s*)"(?:\\.|[^"])*"`)
	lineSecretPattern   = regexp.MustCompile(`(?im)^(\s*[A-Z0-9_.-]*(?:PASSWORD|PASSWD|TOKEN|SECRET|AUTHORIZATION|AUTH|KEY-DATA|CERTIFICATE-DATA|PROXY)[A-Z0-9_.-]*\s*[:=]\s*).+$`)
	inlineSecretPattern = regexp.MustCompile(
		`(?i)\b(password|passwd|token|secret|authorization|auth)\s*[:=]\s*[^,\s;"']+`,
	)
	bearerPattern      = regexp.MustCompile(`(?i)(authorization\s*:\s*bearer\s+)[^\s]+`)
	urlUserPattern     = regexp.MustCompile(`(?i)(https?://)[^/@\s]+@`)
	commonTokenPattern = regexp.MustCompile(
		`(?i)\b(?:gh[pousr]_[A-Za-z0-9_]{16,}|AKIA[A-Z0-9]{16}|xox[baprs]-[A-Za-z0-9-]{16,})\b`,
	)
)

func Redact(input []byte, context RedactionContext) ([]byte, int) {
	output := append([]byte(nil), input...)
	redactions := 0
	output = replaceMatches(output, pemSecretPattern, func([]byte) []byte {
		return []byte(redactedValue)
	}, &redactions)
	output = replaceMatches(output, jsonSecretPattern, func(match []byte) []byte {
		separator := bytes.LastIndex(match, []byte(`"`))
		if separator <= 0 {
			return []byte(redactedValue)
		}
		prefix := match[:separator+1]
		colon := bytes.LastIndex(prefix, []byte(":"))
		if colon < 0 {
			return []byte(redactedValue)
		}
		return append(append([]byte(nil), prefix[:colon+1]...), []byte(` "`+redactedValue+`"`)...)
	}, &redactions)
	output = replaceMatches(output, lineSecretPattern, func(match []byte) []byte {
		separator := bytes.IndexAny(match, ":=")
		if separator < 0 {
			return []byte(redactedValue)
		}
		return append(append([]byte(nil), match[:separator+1]...), []byte(redactedValue)...)
	}, &redactions)
	output = replaceMatches(output, inlineSecretPattern, func(match []byte) []byte {
		separator := bytes.IndexAny(match, ":=")
		if separator < 0 {
			return []byte(redactedValue)
		}
		return append(append([]byte(nil), match[:separator+1]...), []byte(redactedValue)...)
	}, &redactions)
	output = replaceMatches(output, bearerPattern, func(match []byte) []byte {
		index := bytes.LastIndexByte(match, ' ')
		if index < 0 {
			return []byte(redactedValue)
		}
		return append(append([]byte(nil), match[:index+1]...), []byte(redactedValue)...)
	}, &redactions)
	output = replaceMatches(output, urlUserPattern, func(match []byte) []byte {
		scheme := match[:bytes.Index(match, []byte("://"))+3]
		return append(append([]byte(nil), scheme...), []byte(redactedValue+"@")...)
	}, &redactions)
	output = replaceMatches(output, commonTokenPattern, func([]byte) []byte {
		return []byte(redactedValue)
	}, &redactions)

	text := string(output)
	for _, replacement := range []struct {
		value       string
		placeholder string
	}{
		{context.HomeDirectory, "$HOME"},
		{context.Hostname, "$HOSTNAME"},
		{context.Username, "$USER"},
	} {
		value := strings.TrimSpace(replacement.value)
		if value == "" || value == "/" || value == replacement.placeholder {
			continue
		}
		count := strings.Count(text, value)
		if count == 0 {
			continue
		}
		text = strings.ReplaceAll(text, value, replacement.placeholder)
		redactions += count
	}
	return []byte(text), redactions
}

func replaceMatches(
	input []byte,
	pattern *regexp.Regexp,
	replacement func([]byte) []byte,
	count *int,
) []byte {
	matches := pattern.FindAllIndex(input, -1)
	if len(matches) == 0 {
		return input
	}
	var output bytes.Buffer
	offset := 0
	for _, match := range matches {
		output.Write(input[offset:match[0]])
		output.Write(replacement(input[match[0]:match[1]]))
		offset = match[1]
		*count++
	}
	output.Write(input[offset:])
	return output.Bytes()
}
