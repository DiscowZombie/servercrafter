package patch

import (
	"fmt"
	"strings"
)

// UnsetError lists the variables a value references that are not set
type UnsetError struct {
	Names []string
}

func (e *UnsetError) Error() string {
	return "unset environment variables: " + strings.Join(e.Names, ", ")
}

// Interpolate replaces ${NAME} with the value of NAME and ${NAME:-default} with the value of NAME,
// or default when NAME is unset or empty. $$ is a literal $. Unset variables without a default
// are an *UnsetError.
func Interpolate(s string, lookup Lookup) (string, error) {
	var out strings.Builder
	var missing []string
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '$' || i+1 >= len(s) {
			out.WriteByte(c)
			continue
		}
		switch s[i+1] {
		case '$':
			out.WriteByte('$')
			i++
		case '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", fmt.Errorf("unterminated ${ in %q", s)
			}
			expr := s[i+2 : i+2+end]
			name, fallback, hasDefault := strings.Cut(expr, ":-")
			if !validName(name) {
				return "", fmt.Errorf("invalid variable name %q in %q", name, s)
			}
			value, ok := lookup(name)
			switch {
			case ok && value != "":
				out.WriteString(value)
			case hasDefault:
				out.WriteString(fallback)
			case ok:
				// Set but empty, without a default: written as is
			default:
				missing = append(missing, name)
			}
			i += 2 + end
		default:
			out.WriteByte(c)
		}
	}
	if len(missing) > 0 {
		return "", &UnsetError{Names: missing}
	}
	return out.String(), nil
}

func validName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		switch {
		case c == '_', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
