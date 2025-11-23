package patch

import (
	"strings"
)

// patchProperties edits a Java .properties file line by line: the lines of the keys being set are
// rewritten, everything else (comments, order, blank lines) is left untouched and missing keys
// are appended
func patchProperties(data []byte, values []value) ([]byte, error) {
	newline := "\n"
	if strings.Contains(string(data), "\r\n") {
		newline = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	trailingNewline := len(lines) > 0 && lines[len(lines)-1] == ""
	if trailingNewline {
		lines = lines[:len(lines)-1]
	}
	pending := map[string]string{}
	var order []string
	for _, v := range values {
		if _, ok := pending[v.path]; !ok {
			order = append(order, v.path)
		}
		pending[v.path] = formatScalar(v.value)
	}

	var out []string
	for i := 0; i < len(lines); i++ {
		// A logical line continues on the next physical line when it ends with an odd number of
		// backslashes
		start := i
		for i < len(lines)-1 && continues(lines[i]) {
			i++
		}
		first := lines[start]
		trimmed := strings.TrimLeft(first, " \t\f")
		if trimmed == "" || trimmed[0] == '#' || trimmed[0] == '!' {
			out = append(out, lines[start:i+1]...)
			continue
		}
		rawKey, prefix := splitProperty(first)
		key := unescapeProperty(rawKey)
		v, ok := pending[key]
		if !ok {
			out = append(out, lines[start:i+1]...)
			continue
		}
		out = append(out, prefix+escapePropertyValue(v))
		delete(pending, key)
	}
	for _, key := range order {
		if v, ok := pending[key]; ok {
			out = append(out, escapePropertyKey(key)+"="+escapePropertyValue(v))
		}
	}
	result := strings.Join(out, newline)
	if trailingNewline || len(lines) == 0 {
		result += newline
	}
	return []byte(result), nil
}

func continues(line string) bool {
	backslashes := 0
	for i := len(line) - 1; i >= 0 && line[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 == 1
}

// splitProperty returns the raw key of a property line and the part of the line preceding its
// value (indentation, key and separator)
func splitProperty(line string) (string, string) {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t' || line[i] == '\f') {
		i++
	}
	keyStart := i
	for i < len(line) {
		c := line[i]
		if c == '\\' {
			i += 2
			continue
		}
		if c == '=' || c == ':' || c == ' ' || c == '\t' || c == '\f' {
			break
		}
		i++
	}
	if i > len(line) {
		i = len(line)
	}
	key := line[keyStart:i]
	// Separator: whitespace, then at most one of = or :, then whitespace
	for i < len(line) && (line[i] == ' ' || line[i] == '\t' || line[i] == '\f') {
		i++
	}
	if i < len(line) && (line[i] == '=' || line[i] == ':') {
		i++
		for i < len(line) && (line[i] == ' ' || line[i] == '\t' || line[i] == '\f') {
			i++
		}
	} else if i == len(line) {
		// Key without value: give it a separator
		return key, line + "="
	}
	return key, line[:i]
}

func unescapeProperty(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 't':
				b.WriteByte('\t')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 'f':
				b.WriteByte('\f')
			default:
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func escapePropertyKey(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch c {
		case ' ', '=', ':', '#', '!':
			b.WriteByte('\\')
			b.WriteRune(c)
		default:
			b.WriteString(escapeControl(c))
		}
	}
	return b.String()
}

func escapePropertyValue(s string) string {
	var b strings.Builder
	for i, c := range s {
		if c == ' ' && i == 0 {
			b.WriteString(`\ `)
			continue
		}
		b.WriteString(escapeControl(c))
	}
	return b.String()
}

func escapeControl(c rune) string {
	switch c {
	case '\\':
		return `\\`
	case '\n':
		return `\n`
	case '\r':
		return `\r`
	case '\t':
		return `\t`
	case '\f':
		return `\f`
	}
	return string(c)
}
