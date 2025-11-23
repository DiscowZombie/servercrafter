// Package yamlutil decodes servercrafter configuration files strictly: unknown keys are errors, so a
// typo in a template is reported instead of being silently ignored.
package yamlutil

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"go.yaml.in/yaml/v3"
)

// DecodeFile reads path and decodes its single YAML document into out, rejecting unknown fields
func DecodeFile(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err = Decode(data, out); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// Decode decodes a single YAML document into out, rejecting unknown fields
func Decode(data []byte, out any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("empty document")
		}
		return err
	}
	return nil
}
