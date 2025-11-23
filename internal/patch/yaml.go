package patch

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// patchYAML sets dotted paths of a YAML document through its node tree, so that comments, key
// order and key case survive
func patchYAML(data []byte, values []value) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := doc.Content[0]
	if isNull(root) {
		*root = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	for _, v := range values {
		if err := setYAML(root, strings.Split(v.path, "."), v.value, v.path); err != nil {
			return nil, err
		}
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(detectIndent(data))
	if err := encoder.Encode(&doc); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func setYAML(node *yaml.Node, segments []string, v any, fullPath string) error {
	segment := segments[0]
	last := len(segments) == 1
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value != segment {
				continue
			}
			child := node.Content[i+1]
			if last {
				return setYAMLScalar(child, v, fullPath)
			}
			if isNull(child) {
				*child = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			}
			return setYAML(child, segments[1:], v, fullPath)
		}
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: segment}
		child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		node.Content = append(node.Content, key, child)
		if last {
			return setYAMLScalar(child, v, fullPath)
		}
		return setYAML(child, segments[1:], v, fullPath)
	case yaml.SequenceNode:
		index, err := strconv.Atoi(segment)
		if err != nil || index < 0 || index >= len(node.Content) {
			return fmt.Errorf("%s: %q is not an index of the list (0 to %d)", fullPath, segment, len(node.Content)-1)
		}
		if last {
			return setYAMLScalar(node.Content[index], v, fullPath)
		}
		return setYAML(node.Content[index], segments[1:], v, fullPath)
	case yaml.AliasNode:
		return fmt.Errorf("%s: cannot patch through the alias *%s", fullPath, node.Value)
	}
	return fmt.Errorf("%s: %q holds a scalar, not a mapping", fullPath, segment)
}

func setYAMLScalar(node *yaml.Node, v any, fullPath string) error {
	if node.Kind == yaml.MappingNode && len(node.Content) > 0 || node.Kind == yaml.SequenceNode {
		return fmt.Errorf("%s: cannot replace a mapping or a list with a scalar", fullPath)
	}
	style := node.Style
	node.Kind = yaml.ScalarNode
	node.Content = nil
	node.Anchor = ""
	node.Value = formatScalar(v)
	node.Style = 0
	switch v.(type) {
	case string:
		node.Tag = "!!str"
		// Keep the quoting the file used for the value
		if style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0 {
			node.Style = style & (yaml.DoubleQuotedStyle | yaml.SingleQuotedStyle)
		}
	case int64:
		node.Tag = "!!int"
	case float64:
		node.Tag = "!!float"
	case bool:
		node.Tag = "!!bool"
	}
	return nil
}

func isNull(node *yaml.Node) bool {
	return node.Kind == yaml.ScalarNode && (node.Tag == "!!null" || node.Value == "" && node.Tag == "")
}

// detectIndent guesses the indentation of a YAML file from its first indented mapping key
func detectIndent(data []byte) int {
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if indent == 0 || trimmed == "" || trimmed[0] == '#' || trimmed[0] == '-' {
			continue
		}
		if indent >= 2 && indent <= 8 {
			return indent
		}
	}
	return 2
}
