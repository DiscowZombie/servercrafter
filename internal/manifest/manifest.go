// Package manifest records what a build installed, so that bootstrap can verify it before the
// server starts and later builds or diffs know what is already there.
package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/discowzombie/servercrafter/internal/template"
)

// FileName is the name of the manifest written at the root of a built server
const FileName = "servercrafter-manifest.json"

// Kinds of entries
const (
	KindFile = "file"
	KindDir  = "dir"
)

// Manifest describes a built server
type Manifest struct {
	Version        int              `json:"version"`
	Template       string           `json:"template"`
	Tool           string           `json:"servercrafter,omitempty"`
	Entries        []Entry          `json:"entries"`
	RuntimePatches []template.Patch `json:"runtimePatches,omitempty"`
}

// Entry is one installed artifact
type Entry struct {
	// ID is server, plugin:<jarId> or world:<name>
	ID string `json:"id"`
	// Path relative to the server root, slash separated
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Origin  string `json:"origin,omitempty"`
	Version string `json:"version,omitempty"`
	// SHA256 of a file entry
	SHA256 string `json:"sha256,omitempty"`
}

// Path returns where the manifest of a server rooted at dir lives
func Path(dir string) string {
	return filepath.Join(dir, FileName)
}

// Read loads a manifest. Patch values keep their JSON number type so that integers stay integers.
func Read(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	m := &Manifest{}
	if err = decoder.Decode(m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if m.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported manifest version %d", path, m.Version)
	}
	return m, nil
}

// ReadIfExists loads the manifest of dir, nil when dir holds none
func ReadIfExists(dir string) (*Manifest, error) {
	m, err := Read(Path(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return m, err
}

// Write stores the manifest of a server rooted at dir
func (m *Manifest) Write(dir string) error {
	sort.SliceStable(m.Entries, func(i, j int) bool { return m.Entries[i].ID < m.Entries[j].ID })
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := Path(dir) + ".tmp"
	if err = os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, Path(dir))
}

// Find returns the entry with the given id
func (m *Manifest) Find(id string) (Entry, bool) {
	if m == nil {
		return Entry{}, false
	}
	for _, e := range m.Entries {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}
