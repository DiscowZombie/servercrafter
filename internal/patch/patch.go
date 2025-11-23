// Package patch sets fields of configuration files (.properties, YAML, JSON) in place, keeping
// their comments, key order and key case.
package patch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/discowzombie/servercrafter/internal/template"
)

// Lookup returns the value of an environment variable
type Lookup func(name string) (string, bool)

// Apply edits the file a patch targets, below root. String values are interpolated with lookup.
func Apply(root string, p template.Patch, lookup Lookup) error {
	clean := path.Clean("/" + filepath.ToSlash(p.File))
	file := filepath.Join(root, filepath.FromSlash(clean))
	info, err := os.Stat(file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s: no such file, an overlay or a plugin has to provide it", p.File)
		}
		return err
	}
	values := make([]value, 0, len(p.Fields))
	var missing []string
	for _, field := range p.Fields {
		v, err := render(field, lookup)
		var unset *UnsetError
		if errors.As(err, &unset) {
			missing = append(missing, unset.Names...)
			continue
		}
		if err != nil {
			return fmt.Errorf("%s: %s: %w", p.File, field.Field, err)
		}
		values = append(values, value{path: field.Field, value: v})
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s: %w", p.File, &UnsetError{Names: dedupe(missing)})
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var out []byte
	switch strings.ToLower(filepath.Ext(file)) {
	case ".properties":
		out, err = patchProperties(data, values)
	case ".yml", ".yaml":
		out, err = patchYAML(data, values)
	case ".json":
		out, err = patchJSON(data, values)
	default:
		err = fmt.Errorf("unsupported file type %s", filepath.Ext(file))
	}
	if err != nil {
		return fmt.Errorf("%s: %w", p.File, err)
	}
	return writeAtomic(file, out, info.Mode().Perm())
}

type value struct {
	path  string
	value any // string, int64, float64 or bool
}

// render interpolates a field value and converts it to the type it is written as
func render(f template.Field, lookup Lookup) (any, error) {
	raw := f.Value
	if n, ok := raw.(json.Number); ok {
		// Values read back from a build manifest
		if i, err := n.Int64(); err == nil {
			raw = i
		} else if fl, err := n.Float64(); err == nil {
			raw = fl
		}
	}
	if i, ok := raw.(int); ok {
		raw = int64(i)
	}
	if s, ok := raw.(string); ok {
		interpolated, err := Interpolate(s, lookup)
		if err != nil {
			return nil, err
		}
		raw = interpolated
	}
	switch f.Type {
	case "":
		return raw, nil
	case "string":
		return formatScalar(raw), nil
	case "int":
		switch v := raw.(type) {
		case int64:
			return v, nil
		case float64:
			if v == float64(int64(v)) {
				return int64(v), nil
			}
		case string:
			if i, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return i, nil
			}
		}
	case "float":
		switch v := raw.(type) {
		case int64:
			return float64(v), nil
		case float64:
			return v, nil
		case string:
			if fl, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				return fl, nil
			}
		}
	case "bool":
		switch v := raw.(type) {
		case bool:
			return v, nil
		case string:
			if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
				return b, nil
			}
		}
	default:
		return nil, fmt.Errorf("unsupported type %q", f.Type)
	}
	return nil, fmt.Errorf("%q is not a valid %s", formatScalar(raw), f.Type)
}

func formatScalar(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case int64:
		return strconv.FormatInt(value, 10)
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(value)
	}
	return fmt.Sprint(v)
}

func writeAtomic(file string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+".patch-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}

func dedupe(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
