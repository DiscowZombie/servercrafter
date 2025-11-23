package template

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemplates(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestFlattenMergesParentsInOrder(t *testing.T) {
	dir := writeTemplates(t, map[string]string{
		"_base.yaml": `
version: 1
server: { url: https://example.com/paper.jar }
plugins:
  - { jarId: a, name: a.jar, url: https://example.com/a.jar }
  - { jarId: b, name: b.jar, url: https://example.com/b.jar }
  - { jarId: c, name: c.jar, url: https://example.com/c.jar }
files: [files/base]
runtimePatches:
  - file: server.properties
    fields: [{ field: motd, value: "${MOTD}" }]
`,
		"_extra.yaml": `
version: 1
parents: [base]
plugins:
  - { jarId: d, name: d.jar, url: https://example.com/d.jar }
files: [files/extra]
`,
		"_other.yaml": `
version: 1
parents: [base]
files: [files/other]
`,
		"lobby.yaml": `
version: 1
id: lobby
parents: [extra, other]
server: { url: https://example.com/paper-pinned.jar }
plugins:
  - { jarId: b, name: b-dev.jar, source: dev, path: b.jar }
  - { jarId: c, remove: true }
files: [files/lobby]
`,
	})
	loader := NewLoader(dir)
	leaf, err := loader.Find("lobby")
	if err != nil {
		t.Fatal(err)
	}
	flat, err := loader.Flatten(leaf)
	if err != nil {
		t.Fatal(err)
	}
	if flat.Server.URL != "https://example.com/paper-pinned.jar" {
		t.Errorf("server: the child should win, got %s", flat.Server.URL)
	}
	var ids []string
	for _, p := range flat.Plugins {
		ids = append(ids, p.JarID+"="+p.Name)
	}
	if got := strings.Join(ids, ","); got != "a=a.jar,b=b-dev.jar,d=d.jar" {
		t.Errorf("plugins: got %s", got)
	}
	// base is shared by extra and other but applied once
	if got := strings.Join(flat.Files, ","); got != "files/base,files/extra,files/other,files/lobby" {
		t.Errorf("files: got %s", got)
	}
	if len(flat.RuntimePatches) != 1 {
		t.Errorf("runtimePatches: expected 1, got %d", len(flat.RuntimePatches))
	}
	chain, err := loader.Chain(leaf)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(chain, ","); got != "_base.yaml,_extra.yaml,_other.yaml,lobby.yaml" {
		t.Errorf("chain: got %s", got)
	}
	// The parent files must not be modified by the merge
	base, _ := loader.LoadFile(filepath.Join(dir, "_base.yaml"))
	if len(base.Plugins) != 3 || base.Plugins[1].Name != "b.jar" {
		t.Errorf("merging modified a parent: %+v", base.Plugins)
	}
}

func TestFlattenDetectsCycles(t *testing.T) {
	dir := writeTemplates(t, map[string]string{
		"_a.yaml":    "version: 1\nparents: [b]\n",
		"_b.yaml":    "version: 1\nparents: [a]\n",
		"leaf.yaml":  "version: 1\nid: leaf\nparents: [a]\n",
		"_self.yaml": "version: 1\n",
		"self.yaml":  "version: 1\nid: self\nparents: [self]\nserver: { url: https://example.com/s.jar }\n",
	})
	loader := NewLoader(dir)
	leaf, err := loader.Find("leaf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = loader.Flatten(leaf); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected a cycle error, got %v", err)
	}
	// A leaf naming its own name as parent refers to the _ partial
	self, err := loader.Find("self")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = loader.Flatten(self); err != nil {
		t.Fatalf("self parent should resolve to _self.yaml: %v", err)
	}
}

func TestRemoveUnknownPluginFails(t *testing.T) {
	dir := writeTemplates(t, map[string]string{
		"leaf.yaml": "version: 1\nid: leaf\nserver: { url: https://example.com/s.jar }\nplugins: [{ jarId: x, remove: true }]\n",
	})
	loader := NewLoader(dir)
	leaf, err := loader.Find("leaf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = loader.Flatten(leaf); err == nil || !strings.Contains(err.Error(), "cannot remove") {
		t.Fatalf("expected a remove error, got %v", err)
	}
}

func TestFindReportsAvailableTemplates(t *testing.T) {
	dir := writeTemplates(t, map[string]string{
		"a.yaml": "version: 1\nid: a\n",
		"b.yaml": "version: 1\nid: b\n",
	})
	_, err := NewLoader(dir).Find("c")
	if err == nil || !strings.Contains(err.Error(), "available: a, b") {
		t.Fatalf("got %v", err)
	}
}

func TestDuplicateIDs(t *testing.T) {
	dir := writeTemplates(t, map[string]string{
		"a.yaml": "version: 1\nid: same\n",
		"b.yaml": "version: 1\nid: same\n",
	})
	if _, err := NewLoader(dir).Buildable(); err == nil || !strings.Contains(err.Error(), "both declare") {
		t.Fatalf("got %v", err)
	}
}
