package patch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// jsonNode is a JSON value keeping the order of object keys, which encoding/json maps lose
type jsonNode struct {
	kind   byte // 'o' object, 'a' array, 'v' scalar
	keys   []string
	values []*jsonNode // object values or array items
	scalar any         // string, json.Number, bool or nil
}

func patchJSON(data []byte, values []value) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	root := &jsonNode{kind: 'o'}
	if len(bytes.TrimSpace(data)) > 0 {
		var err error
		if root, err = readJSON(decoder); err != nil {
			return nil, err
		}
		if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
			return nil, errors.New("trailing data after the JSON document")
		}
	}
	for _, v := range values {
		if err := setJSON(root, strings.Split(v.path, "."), v.value, v.path); err != nil {
			return nil, err
		}
	}
	var out bytes.Buffer
	if err := writeJSON(&out, root, detectJSONIndent(data), 0); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

func readJSON(decoder *json.Decoder) (*jsonNode, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch t := token.(type) {
	case json.Delim:
		switch t {
		case '{':
			node := &jsonNode{kind: 'o'}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, fmt.Errorf("invalid object key %v", keyToken)
				}
				child, err := readJSON(decoder)
				if err != nil {
					return nil, err
				}
				node.keys = append(node.keys, key)
				node.values = append(node.values, child)
			}
			_, err = decoder.Token()
			return node, err
		case '[':
			node := &jsonNode{kind: 'a'}
			for decoder.More() {
				child, err := readJSON(decoder)
				if err != nil {
					return nil, err
				}
				node.values = append(node.values, child)
			}
			_, err = decoder.Token()
			return node, err
		}
		return nil, fmt.Errorf("unexpected %v", t)
	default:
		return &jsonNode{kind: 'v', scalar: t}, nil
	}
}

func setJSON(node *jsonNode, segments []string, v any, fullPath string) error {
	segment := segments[0]
	last := len(segments) == 1
	switch node.kind {
	case 'o':
		for i, key := range node.keys {
			if key != segment {
				continue
			}
			child := node.values[i]
			if last {
				return setJSONScalar(child, v, fullPath)
			}
			if child.kind == 'v' && child.scalar == nil {
				*child = jsonNode{kind: 'o'}
			}
			return setJSON(child, segments[1:], v, fullPath)
		}
		child := &jsonNode{kind: 'o'}
		node.keys = append(node.keys, segment)
		node.values = append(node.values, child)
		if last {
			return setJSONScalar(child, v, fullPath)
		}
		return setJSON(child, segments[1:], v, fullPath)
	case 'a':
		index, err := strconv.Atoi(segment)
		if err != nil || index < 0 || index >= len(node.values) {
			return fmt.Errorf("%s: %q is not an index of the array (0 to %d)", fullPath, segment, len(node.values)-1)
		}
		if last {
			return setJSONScalar(node.values[index], v, fullPath)
		}
		return setJSON(node.values[index], segments[1:], v, fullPath)
	}
	return fmt.Errorf("%s: %q holds a scalar, not an object", fullPath, segment)
}

func setJSONScalar(node *jsonNode, v any, fullPath string) error {
	if node.kind == 'a' || node.kind == 'o' && len(node.keys) > 0 {
		return fmt.Errorf("%s: cannot replace an object or an array with a scalar", fullPath)
	}
	*node = jsonNode{kind: 'v', scalar: v}
	return nil
}

func writeJSON(out *bytes.Buffer, node *jsonNode, indent string, level int) error {
	newline := func(l int) {
		if indent != "" {
			out.WriteByte('\n')
			out.WriteString(strings.Repeat(indent, l))
		}
	}
	separator := ":"
	if indent != "" {
		separator = ": "
	}
	switch node.kind {
	case 'o':
		if len(node.keys) == 0 {
			out.WriteString("{}")
			return nil
		}
		out.WriteByte('{')
		for i, key := range node.keys {
			if i > 0 {
				out.WriteByte(',')
			}
			newline(level + 1)
			if err := writeJSONScalar(out, key); err != nil {
				return err
			}
			out.WriteString(separator)
			if err := writeJSON(out, node.values[i], indent, level+1); err != nil {
				return err
			}
		}
		newline(level)
		out.WriteByte('}')
	case 'a':
		if len(node.values) == 0 {
			out.WriteString("[]")
			return nil
		}
		out.WriteByte('[')
		for i, item := range node.values {
			if i > 0 {
				out.WriteByte(',')
			}
			newline(level + 1)
			if err := writeJSON(out, item, indent, level+1); err != nil {
				return err
			}
		}
		newline(level)
		out.WriteByte(']')
	default:
		return writeJSONScalar(out, node.scalar)
	}
	return nil
}

func writeJSONScalar(out *bytes.Buffer, v any) error {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return err
	}
	out.Write(bytes.TrimRight(buf.Bytes(), "\n"))
	return nil
}

// detectJSONIndent keeps the indentation of the file: tabs, a number of spaces, or none for a
// minified file
func detectJSONIndent(data []byte) string {
	lines := strings.Split(string(data), "\n")
	if len(bytes.TrimSpace(data)) > 0 && len(lines) <= 2 {
		return ""
	}
	for _, line := range lines[1:] {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || len(trimmed) == len(line) {
			continue
		}
		return line[:len(line)-len(trimmed)]
	}
	return "  "
}
