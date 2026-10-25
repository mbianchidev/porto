package docker

import "strings"

func windowsCommandLine(arguments []string) string {
	quoted := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		if argument != "" && !strings.ContainsAny(argument, " \t\"") {
			quoted = append(quoted, argument)
			continue
		}
		var output strings.Builder
		output.WriteByte('"')
		slashes := 0
		for _, character := range argument {
			if character == '\\' {
				slashes++
				continue
			}
			if character == '"' {
				output.WriteString(strings.Repeat(`\`, slashes*2+1))
				output.WriteRune(character)
			} else {
				output.WriteString(strings.Repeat(`\`, slashes))
				output.WriteRune(character)
			}
			slashes = 0
		}
		output.WriteString(strings.Repeat(`\`, slashes*2))
		output.WriteByte('"')
		quoted = append(quoted, output.String())
	}
	return strings.Join(quoted, " ")
}
