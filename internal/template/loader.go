package template

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/discowzombie/servercrafter/internal/yamlutil"
)

var templateExtensions = []string{".yaml", ".yml"}

// Loader reads templates from a directory and caches them by path
type Loader struct {
	Dir   string
	cache map[string]*Template
}

// NewLoader returns a loader of the templates stored in dir
func NewLoader(dir string) *Loader {
	return &Loader{Dir: dir, cache: map[string]*Template{}}
}

// LoadFile reads and validates a single template file, without its parents
func (l *Loader) LoadFile(path string) (*Template, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if t, ok := l.cache[abs]; ok {
		return t, nil
	}
	t := &Template{}
	if err = yamlutil.DecodeFile(abs, t); err != nil {
		return nil, err
	}
	t.Path = abs
	if err = t.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", l.rel(abs), err)
	}
	l.cache[abs] = t
	return t, nil
}

// All loads every template of the directory, partials included, sorted by file name
func (l *Loader) All() ([]*Template, error) {
	entries, err := os.ReadDir(l.Dir)
	if err != nil {
		return nil, err
	}
	var templates []*Template
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !hasTemplateExtension(entry.Name()) {
			continue
		}
		t, err := l.LoadFile(filepath.Join(l.Dir, entry.Name()))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		templates = append(templates, t)
	}
	sort.Slice(templates, func(i, j int) bool { return templates[i].Path < templates[j].Path })
	return templates, errors.Join(errs...)
}

// Buildable returns the templates carrying an id, failing when two of them share one
func (l *Loader) Buildable() ([]*Template, error) {
	all, err := l.All()
	if err != nil {
		return nil, err
	}
	var buildable []*Template
	byID := map[string]string{}
	for _, t := range all {
		if !t.Buildable() {
			continue
		}
		if other, ok := byID[t.ID]; ok {
			return nil, fmt.Errorf("templates %s and %s both declare id %q", l.rel(other), l.rel(t.Path), t.ID)
		}
		byID[t.ID] = t.Path
		buildable = append(buildable, t)
	}
	return buildable, nil
}

// Find returns the buildable template with the given id. A path to a template file is accepted
// as well, so that a template can be built outside any project.
func (l *Loader) Find(ref string) (*Template, error) {
	if hasTemplateExtension(ref) {
		if _, err := os.Stat(ref); err == nil {
			t, err := l.LoadFile(ref)
			if err != nil {
				return nil, err
			}
			if !t.Buildable() {
				return nil, fmt.Errorf("%s is a partial template (no id), it can only be used as a parent", ref)
			}
			return t, nil
		}
	}
	all, err := l.Buildable()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, t := range all {
		if t.ID == ref {
			return t, nil
		}
		ids = append(ids, t.ID)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no template with id %q: %s holds no buildable template", ref, l.Dir)
	}
	return nil, fmt.Errorf("no template with id %q (available: %s)", ref, strings.Join(ids, ", "))
}

// Flatten merges the parents of t into a single template. Parents apply in the order they are
// listed, depth first, each ancestor once, and t itself last:
//   - server: the last one declared wins
//   - plugins and worlds: merged by jarId and name, a later entry replaces an earlier one in place
//     and an entry with remove: true drops it
//   - files, patches and runtimePatches: concatenated
func (l *Loader) Flatten(t *Template) (*Template, error) {
	var chain []*Template
	if err := l.linearize(t, nil, map[string]bool{}, &chain); err != nil {
		return nil, err
	}
	merged := &Template{Version: 1, ID: t.ID, Path: t.Path}
	for _, layer := range chain {
		if err := merged.apply(layer); err != nil {
			return nil, fmt.Errorf("%s: %w", l.rel(layer.Path), err)
		}
	}
	return merged, nil
}

// Chain returns the files t is built from, in the order they are applied
func (l *Loader) Chain(t *Template) ([]string, error) {
	var chain []*Template
	if err := l.linearize(t, nil, map[string]bool{}, &chain); err != nil {
		return nil, err
	}
	paths := make([]string, len(chain))
	for i, layer := range chain {
		paths[i] = l.rel(layer.Path)
	}
	return paths, nil
}

func (l *Loader) linearize(t *Template, stack []string, seen map[string]bool, out *[]*Template) error {
	stack = append(stack, t.Path)
	for _, name := range t.Parents {
		path, err := l.parentPath(filepath.Dir(t.Path), name)
		if err != nil {
			return fmt.Errorf("%s: parent %q: %w", l.rel(t.Path), name, err)
		}
		for _, visiting := range stack {
			if visiting == path {
				cycle := make([]string, 0, len(stack)+1)
				for _, p := range stack {
					cycle = append(cycle, l.rel(p))
				}
				return fmt.Errorf("parent cycle: %s -> %s", strings.Join(cycle, " -> "), l.rel(path))
			}
		}
		if seen[path] {
			continue
		}
		parent, err := l.LoadFile(path)
		if err != nil {
			return err
		}
		if err = l.linearize(parent, stack, seen, out); err != nil {
			return err
		}
	}
	seen[t.Path] = true
	*out = append(*out, t)
	return nil
}

// parentPath finds the file a parent name refers to: the partial _<name> first, then <name>
func (l *Loader) parentPath(dir string, name string) (string, error) {
	if !namePattern.MatchString(name) {
		return "", fmt.Errorf("invalid template name")
	}
	var tried []string
	for _, prefix := range []string{"_", ""} {
		for _, ext := range templateExtensions {
			candidate := filepath.Join(dir, prefix+name+ext)
			if _, err := os.Stat(candidate); err == nil {
				return filepath.Abs(candidate)
			}
			tried = append(tried, prefix+name+ext)
		}
	}
	return "", fmt.Errorf("not found in %s (tried %s)", l.rel(dir), strings.Join(tried, ", "))
}

func (l *Loader) rel(path string) string {
	if rel, err := filepath.Rel(l.Dir, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

func hasTemplateExtension(name string) bool {
	for _, ext := range templateExtensions {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

func (t *Template) apply(layer *Template) error {
	if layer.Server != nil {
		server := *layer.Server
		t.Server = &server
	}
	for _, plugin := range layer.Plugins {
		index := -1
		for i := range t.Plugins {
			if t.Plugins[i].JarID == plugin.JarID {
				index = i
				break
			}
		}
		switch {
		case plugin.Remove && index < 0:
			return fmt.Errorf("plugins: cannot remove %q, no parent declares it", plugin.JarID)
		case plugin.Remove:
			t.Plugins = append(t.Plugins[:index], t.Plugins[index+1:]...)
		case index >= 0:
			t.Plugins[index] = plugin
		default:
			t.Plugins = append(t.Plugins, plugin)
		}
	}
	for _, world := range layer.Worlds {
		index := -1
		for i := range t.Worlds {
			if t.Worlds[i].Name == world.Name {
				index = i
				break
			}
		}
		switch {
		case world.Remove && index < 0:
			return fmt.Errorf("worlds: cannot remove %q, no parent declares it", world.Name)
		case world.Remove:
			t.Worlds = append(t.Worlds[:index], t.Worlds[index+1:]...)
		case index >= 0:
			t.Worlds[index] = world
		default:
			t.Worlds = append(t.Worlds, world)
		}
	}
	t.Files = append(t.Files, layer.Files...)
	t.Patches = append(t.Patches, layer.Patches...)
	t.RuntimePatches = append(t.RuntimePatches, layer.RuntimePatches...)
	return nil
}
