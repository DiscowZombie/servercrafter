package patch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discowzombie/servercrafter/internal/template"
)

func env(vars map[string]string) Lookup {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

func apply(t *testing.T, file string, content string, fields []template.Field, vars map[string]string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	err := Apply(dir, template.Patch{File: file, Fields: fields}, env(vars))
	out, _ := os.ReadFile(filepath.Join(dir, file))
	if info, statErr := os.Stat(filepath.Join(dir, file)); statErr == nil && info.Mode().Perm() != 0o640 {
		t.Errorf("file mode changed to %v", info.Mode().Perm())
	}
	return string(out), err
}

func TestInterpolate(t *testing.T) {
	lookup := env(map[string]string{"A": "1", "EMPTY": ""})
	tests := map[string]string{
		"${A}":              "1",
		"x-${A}-${A}":       "x-1-1",
		"${B:-def}":         "def",
		"${EMPTY:-def}":     "def",
		"${EMPTY}":          "",
		"$$${A}":            "$1",
		"cost: 5$":          "cost: 5$",
		"$HOME stays plain": "$HOME stays plain",
	}
	for in, want := range tests {
		if got, err := Interpolate(in, lookup); err != nil || got != want {
			t.Errorf("Interpolate(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	_, err := Interpolate("${B}${C}", lookup)
	var unset *UnsetError
	if !errors.As(err, &unset) || strings.Join(unset.Names, ",") != "B,C" {
		t.Errorf("expected B and C unset, got %v", err)
	}
	if _, err = Interpolate("${A", lookup); err == nil {
		t.Error("expected an unterminated error")
	}
}

func TestProperties(t *testing.T) {
	in := "#Minecraft server properties\nmotd=A Minecraft Server\nmax-players = 20\nlevel-name: world\n\nonline-mode=true\n"
	out, err := apply(t, "server.properties", in, []template.Field{
		{Field: "max-players", Value: 50},
		{Field: "motd", Value: "${MOTD}"},
		{Field: "level-name", Value: "lobby"},
		{Field: "server-port", Value: "25566"},
	}, map[string]string{"MOTD": "  Hello\\World"})
	if err != nil {
		t.Fatal(err)
	}
	want := "#Minecraft server properties\nmotd=\\  Hello\\\\World\nmax-players = 50\nlevel-name: lobby\n\nonline-mode=true\nserver-port=25566\n"
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestPropertiesContinuation(t *testing.T) {
	in := "a=1\\\n  2\nb=3\n"
	out, err := apply(t, "x.properties", in, []template.Field{{Field: "a", Value: "x"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "a=x\nb=3\n" {
		t.Errorf("got %q", out)
	}
}

func TestYAMLKeepsCommentsCaseAndOrder(t *testing.T) {
	in := `# Paper global configuration
proxies:
  velocity:
    enabled: false # toggled by servercrafter
    online-mode: true
    secret: ''
chunk-system:
  ioThreads: -1
`
	out, err := apply(t, "paper-global.yml", in, []template.Field{
		{Field: "proxies.velocity.enabled", Value: true},
		{Field: "proxies.velocity.secret", Value: "${SECRET}"},
		{Field: "chunk-system.ioThreads", Value: "4", Type: "int"},
		{Field: "new.nested.key", Value: 1.5},
	}, map[string]string{"SECRET": "123"})
	if err != nil {
		t.Fatal(err)
	}
	want := `# Paper global configuration
proxies:
  velocity:
    enabled: true # toggled by servercrafter
    online-mode: true
    secret: '123'
chunk-system:
  ioThreads: 4
new:
  nested:
    key: 1.5
`
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestYAMLErrors(t *testing.T) {
	if _, err := apply(t, "a.yml", "a:\n  b: 1\n", []template.Field{{Field: "a", Value: "x"}}, nil); err == nil ||
		!strings.Contains(err.Error(), "cannot replace a mapping") {
		t.Errorf("replacing a mapping: %v", err)
	}
	if _, err := apply(t, "a.yml", "a: 1\n", []template.Field{{Field: "a.b", Value: "x"}}, nil); err == nil ||
		!strings.Contains(err.Error(), "holds a scalar") {
		t.Errorf("descending into a scalar: %v", err)
	}
	if _, err := apply(t, "a.yml", "a: 1\n", []template.Field{{Field: "a", Value: "${X}"}, {Field: "b", Value: "${Y}"}}, nil); err == nil ||
		!strings.Contains(err.Error(), "X, Y") {
		t.Errorf("unset variables: %v", err)
	}
	if _, err := apply(t, "a.yml", "a: 1\n", []template.Field{{Field: "a", Value: "nope", Type: "int"}}, nil); err == nil {
		t.Error("expected an invalid int error")
	}
}

func TestYAMLSequenceIndex(t *testing.T) {
	out, err := apply(t, "a.yaml", "servers:\n  - name: a\n  - name: b\n", []template.Field{{Field: "servers.1.name", Value: "c"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "servers:\n  - name: a\n  - name: c\n" {
		t.Errorf("got %q", out)
	}
}

func TestJSONKeepsOrderAndTypes(t *testing.T) {
	in := "{\n    \"zeta\": 1,\n    \"alpha\": {\"url\": \"<x>\", \"n\": 1.0},\n    \"list\": [1, 2]\n}\n"
	out, err := apply(t, "config.json", in, []template.Field{
		{Field: "alpha.url", Value: "https://a&b"},
		{Field: "list.0", Value: json.Number("3")},
		{Field: "token", Value: "${T}"},
	}, map[string]string{"T": "t0k"})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n    \"zeta\": 1,\n    \"alpha\": {\n        \"url\": \"https://a&b\",\n        \"n\": 1.0\n    },\n    \"list\": [\n        3,\n        2\n    ],\n    \"token\": \"t0k\"\n}\n"
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestMissingFile(t *testing.T) {
	err := Apply(t.TempDir(), template.Patch{File: "nope.yml", Fields: []template.Field{{Field: "a", Value: 1}}}, env(nil))
	if err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("got %v", err)
	}
}
