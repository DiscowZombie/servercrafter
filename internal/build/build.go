// Package build installs a flattened template into a server directory: artifacts first, in
// parallel, then overlays, then build time patches, and finally the manifest.
package build

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/discowzombie/servercrafter/internal/fetch"
	"github.com/discowzombie/servercrafter/internal/manifest"
	"github.com/discowzombie/servercrafter/internal/patch"
	"github.com/discowzombie/servercrafter/internal/project"
	"github.com/discowzombie/servercrafter/internal/resolve"
	"github.com/discowzombie/servercrafter/internal/template"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

// Options of a build
type Options struct {
	Project  *project.Project
	Template *template.Template // flattened
	Resolver *resolve.Resolver
	OutDir   string
	// Overrides maps server, a jarId or world:<name> to the version to install instead
	Overrides map[string]string
	Jobs      int
	// Clean empties the output directory first, when servercrafter built it
	Clean bool
	// Refresh downloads every artifact again, even the ones already installed
	Refresh bool
	Env     patch.Lookup
	Log     logrus.FieldLogger
	Tool    string
}

// Item is an artifact of the template to install
type Item struct {
	ID       string
	Path     string // relative to the server root, slash separated
	Artifact *template.Artifact
	World    *template.World
}

// Plan lists what a template installs, once version overrides and required environment
// variables are taken into account
func Plan(t *template.Template, overrides map[string]string, env patch.Lookup, log logrus.FieldLogger) ([]Item, error) {
	var items []Item
	used := map[string]bool{}
	add := func(id string, key string, p string, a template.Artifact, world *template.World) error {
		if a.RequiredEnv != "" {
			if value, _ := env(a.RequiredEnv); value != "true" && value != "1" {
				log.Infof("Skipping %s: %s is not set to true", id, a.RequiredEnv)
				return nil
			}
		}
		if a.Download != nil {
			// The flattened template shares its downloads with the files it was read from
			d := *a.Download
			a.Download = &d
		}
		overridden := false
		for _, k := range []string{key, id} {
			version, ok := overrides[k]
			if !ok {
				continue
			}
			used[k] = true
			overridden = true
			if a.Download == nil {
				return fmt.Errorf("%s: its version cannot be overridden, it is pinned by url or path", id)
			}
			if a.Download.Type == template.DownloadLuckPerms {
				return fmt.Errorf("%s: LuckPerms only publishes its latest build", id)
			}
			log.Infof("Overriding the version of %s: %s", id, version)
			a.Download.Version = version
			a.Download.Build = 0
		}
		if !overridden && a.Download != nil && a.Download.Version != "" {
			// The version may reference environment variables: ${BRANCH:-main}
			version, err := patch.Interpolate(a.Download.Version, env)
			if err != nil {
				return fmt.Errorf("%s: version: %w", id, err)
			}
			a.Download.Version = version
		}
		items = append(items, Item{ID: id, Path: p, Artifact: &a, World: world})
		return nil
	}
	if t.Server == nil {
		return nil, errors.New("the template declares no server")
	}
	if err := add("server", "server", path.Join(t.Server.Destination, t.ServerName()), *t.Server, nil); err != nil {
		return nil, err
	}
	for _, plugin := range t.Plugins {
		dest := plugin.Destination
		if dest == "" {
			dest = template.DefaultPluginsDir
		}
		if err := add("plugin:"+plugin.JarID, plugin.JarID, path.Join(dest, plugin.Name), plugin.Artifact, nil); err != nil {
			return nil, err
		}
	}
	for i := range t.Worlds {
		world := t.Worlds[i]
		id := "world:" + world.Name
		if err := add(id, id, path.Join(world.Destination, world.Name), world.Artifact, &world); err != nil {
			return nil, err
		}
	}
	var unknown []string
	for key := range overrides {
		if !used[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("version overrides match no artifact of the template: %s", strings.Join(unknown, ", "))
	}
	seen := map[string]string{}
	for _, item := range items {
		if other, ok := seen[item.Path]; ok {
			return nil, fmt.Errorf("%s and %s are both installed as %s", other, item.ID, item.Path)
		}
		seen[item.Path] = item.ID
	}
	return items, nil
}

// Run builds the template into the output directory and returns the manifest it wrote
func Run(ctx context.Context, opts Options) (*manifest.Manifest, error) {
	log := opts.Log
	t := opts.Template
	if err := t.ValidateBuildable(); err != nil {
		return nil, err
	}
	if err := t.CheckProject(opts.Project); err != nil {
		return nil, err
	}
	items, err := Plan(t, opts.Overrides, opts.Env, log)
	if err != nil {
		return nil, err
	}
	if opts.Clean {
		if err = clean(opts.OutDir); err != nil {
			return nil, err
		}
	}
	if err = os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, err
	}
	previous, err := manifest.ReadIfExists(opts.OutDir)
	if err != nil {
		log.WithError(err).Warn("Ignoring the manifest of the previous build")
		previous = nil
	}

	jobs := opts.Jobs
	if jobs < 1 {
		jobs = 4
	}
	entries := make([]manifest.Entry, len(items))
	var mu sync.Mutex
	var failures []error
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(jobs)
	for i := range items {
		item := items[i]
		group.Go(func() error {
			entry, err := install(groupCtx, opts, item, previous)
			if err != nil {
				err = fmt.Errorf("%s: %w", item.ID, err)
				mu.Lock()
				failures = append(failures, err)
				mu.Unlock()
				return err
			}
			entries[i] = entry
			return nil
		})
	}
	if err = group.Wait(); err != nil {
		// Report the failures themselves, not the cancellations they caused
		var real []error
		for _, failure := range failures {
			if !errors.Is(failure, context.Canceled) {
				real = append(real, failure)
			}
		}
		if len(real) == 0 {
			real = failures
		}
		return nil, errors.Join(real...)
	}
	removeStale(opts.OutDir, previous, entries, log)

	for _, dir := range t.Files {
		log.Infof("Copying overlay %s", dir)
		if err = fetch.CopyDir(opts.Project.Resolve(dir), opts.OutDir); err != nil {
			return nil, fmt.Errorf("overlay %s: %w", dir, err)
		}
	}
	for _, p := range t.Patches {
		log.Infof("Patching %s", p.File)
		if err = patch.Apply(opts.OutDir, p, opts.Env); err != nil {
			return nil, fmt.Errorf("patch: %w", err)
		}
	}
	for _, p := range t.RuntimePatches {
		if _, err := os.Stat(filepath.Join(opts.OutDir, filepath.FromSlash(p.File))); err != nil {
			log.Warnf("Runtime patch target %s does not exist yet: bootstrap will fail unless something creates it", p.File)
		}
	}

	m := &manifest.Manifest{
		Version:        1,
		Template:       t.ID,
		Tool:           opts.Tool,
		Entries:        entries,
		RuntimePatches: t.RuntimePatches,
	}
	if err = m.Write(opts.OutDir); err != nil {
		return nil, fmt.Errorf("write manifest: %w", err)
	}
	return m, nil
}

func install(ctx context.Context, opts Options, item Item, previous *manifest.Manifest) (manifest.Entry, error) {
	log := opts.Log.WithField("artifact", item.ID)
	entry := manifest.Entry{ID: item.ID, Path: item.Path, Kind: manifest.KindFile}
	target := filepath.Join(opts.OutDir, filepath.FromSlash(item.Path))
	if item.World != nil {
		entry.Kind = manifest.KindDir
		if _, err := os.Stat(target); err == nil && !item.World.Overwrite {
			log.Infof("Keeping the existing world %s (set overwrite: true to replace it)", item.Path)
			if prev, ok := previous.Find(item.ID); ok && prev.Path == item.Path {
				return prev, nil
			}
			return entry, nil
		}
	}

	res, err := opts.Resolver.Resolve(ctx, item.Artifact)
	if err != nil {
		return entry, err
	}
	entry.Origin, entry.Version = res.Origin, res.Version
	if item.World != nil {
		entry.SHA256, err = installWorld(ctx, item, res, target)
		if err == nil {
			log.Infof("Installed %s %s", item.Path, versionLabel(res.Version))
		}
		return entry, err
	}
	if res.Dir != "" {
		return entry, fmt.Errorf("%s is a directory, not a file", res.Origin)
	}

	if !opts.Refresh {
		if sum, ok := upToDate(target, res, previous, item); ok {
			log.Debugf("%s is up to date", item.Path)
			entry.SHA256 = sum
			return entry, nil
		}
	}
	log.Debugf("Downloading %s from %s", item.Path, res.Origin)
	if entry.SHA256, err = fetch.WriteFile(ctx, target, res.Open, res.Checksums); err != nil {
		return entry, err
	}
	log.Infof("Installed %s %s", item.Path, versionLabel(res.Version))
	return entry, nil
}

func versionLabel(version string) string {
	if version == "" {
		return ""
	}
	return "(" + version + ")"
}

// upToDate reports whether the file already installed is the one resolved: it matches the
// published checksums, or the same version of the same origin was installed by the previous build
// and has not been modified since
func upToDate(target string, res *resolve.Resolved, previous *manifest.Manifest, item Item) (string, bool) {
	if _, err := os.Stat(target); err != nil {
		return "", false
	}
	sums, err := hashFile(target, res.Checksums)
	if err != nil {
		return "", false
	}
	if len(res.Checksums) > 0 {
		for algorithm, expected := range res.Checksums {
			if sums[algorithm] != expected {
				return "", false
			}
		}
		return sums["sha256"], true
	}
	prev, ok := previous.Find(item.ID)
	if ok && res.Version != "" && prev.Path == item.Path && prev.Origin == res.Origin &&
		prev.Version == res.Version && prev.SHA256 == sums["sha256"] {
		return sums["sha256"], true
	}
	return "", false
}

func hashFile(file string, checks fetch.Checksums) (map[string]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	hashes := map[string]hash.Hash{"sha256": sha256.New()}
	for algorithm := range checks {
		switch algorithm {
		case "sha1":
			hashes[algorithm] = sha1.New()
		case "sha512":
			hashes[algorithm] = sha512.New()
		}
	}
	writers := make([]io.Writer, 0, len(hashes))
	for _, h := range hashes {
		writers = append(writers, h)
	}
	if _, err = io.Copy(io.MultiWriter(writers...), f); err != nil {
		return nil, err
	}
	sums := map[string]string{}
	for algorithm, h := range hashes {
		sums[algorithm] = hex.EncodeToString(h.Sum(nil))
	}
	return sums, nil
}

// installWorld assembles the world next to its target, then swaps it in
func installWorld(ctx context.Context, item Item, res *resolve.Resolved, target string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(filepath.Dir(target), "."+filepath.Base(target)+".staging-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	content := filepath.Join(staging, "world")
	var sum string
	switch {
	case res.Dir != "":
		if item.World.Extract != template.ExtractNone {
			return "", fmt.Errorf("%s is a directory, it cannot be extracted", res.Origin)
		}
		if err = fetch.CopyDir(res.Dir, content); err != nil {
			return "", err
		}
	case item.World.Extract == template.ExtractZip:
		archive := filepath.Join(staging, "archive.zip")
		if sum, err = fetch.WriteFile(ctx, archive, res.Open, res.Checksums); err != nil {
			return "", err
		}
		if err = fetch.Unzip(archive, content, item.World.StripComponents); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("%s is a file: set extract: zip to unpack it", res.Origin)
	}
	if err = os.RemoveAll(target); err != nil {
		return "", err
	}
	return sum, os.Rename(content, target)
}

// removeStale deletes the files the previous build installed that this one no longer does.
// Worlds are never deleted: they hold the data of the server.
func removeStale(outDir string, previous *manifest.Manifest, entries []manifest.Entry, log logrus.FieldLogger) {
	if previous == nil {
		return
	}
	current := map[string]bool{}
	for _, e := range entries {
		current[e.Path] = true
	}
	for _, prev := range previous.Entries {
		if current[prev.Path] {
			continue
		}
		file := filepath.Join(outDir, filepath.FromSlash(prev.Path))
		if prev.Kind == manifest.KindDir {
			if _, err := os.Stat(file); err == nil {
				log.Warnf("%s is no longer part of the template, delete it by hand if it is not needed", prev.Path)
			}
			continue
		}
		if err := os.Remove(file); err == nil {
			log.Infof("Removed %s, no longer part of the template", prev.Path)
		} else if !errors.Is(err, os.ErrNotExist) {
			log.WithError(err).Warnf("Unable to remove %s", prev.Path)
		}
	}
}

// clean empties a directory servercrafter built, refusing to touch anything else
func clean(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	if _, err = os.Stat(manifest.Path(dir)); err != nil {
		return fmt.Errorf("refusing to clean %s: it holds no %s, servercrafter did not build it", dir, manifest.FileName)
	}
	for _, entry := range entries {
		if err = os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
