// Package bootstrap prepares a built server right before it starts, typically as the entrypoint
// of its container: it verifies the installed files against the manifest, applies the runtime
// patches with the environment of the container, then hands over to the server process.
package bootstrap

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/discowzombie/servercrafter/internal/fetch"
	"github.com/discowzombie/servercrafter/internal/manifest"
	"github.com/discowzombie/servercrafter/internal/patch"
	"github.com/sirupsen/logrus"
)

// Options of a bootstrap
type Options struct {
	Target   string
	Manifest string
	Verify   bool
	Env      patch.Lookup
	Log      logrus.FieldLogger
}

// Prepare verifies and patches the server
func Prepare(opts Options) error {
	manifestPath := opts.Manifest
	if manifestPath == "" {
		manifestPath = manifest.Path(opts.Target)
	}
	m, err := manifest.Read(manifestPath)
	if err != nil {
		return err
	}
	if opts.Verify {
		if err = Verify(opts.Target, m); err != nil {
			return err
		}
		opts.Log.Infof("Verified %d files of %s", countFiles(m), m.Template)
	}
	var errs []error
	for _, p := range m.RuntimePatches {
		if err = patch.Apply(opts.Target, p, opts.Env); err != nil {
			errs = append(errs, err)
			continue
		}
		opts.Log.Debugf("Patched %s", p.File)
	}
	if err = errors.Join(errs...); err != nil {
		return fmt.Errorf("runtime patches: %w", err)
	}
	if len(m.RuntimePatches) > 0 {
		opts.Log.Infof("Applied %d runtime patches", len(m.RuntimePatches))
	}
	return nil
}

// Verify checks every file of the manifest is present with the content the build installed
func Verify(target string, m *manifest.Manifest) error {
	var errs []error
	for _, entry := range m.Entries {
		if entry.Kind != manifest.KindFile || entry.SHA256 == "" {
			continue
		}
		sum, err := fetch.FileSHA256(filepath.Join(target, filepath.FromSlash(entry.Path)))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", entry.Path, err))
			continue
		}
		if sum != entry.SHA256 {
			errs = append(errs, fmt.Errorf("%s: sha256 is %s, the build installed %s", entry.Path, sum, entry.SHA256))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}
	return nil
}

func countFiles(m *manifest.Manifest) int {
	n := 0
	for _, entry := range m.Entries {
		if entry.Kind == manifest.KindFile && entry.SHA256 != "" {
			n++
		}
	}
	return n
}

// Exec replaces the current process with command, so that the server receives the signals sent
// to the container directly
func Exec(dir string, command []string) error {
	binary, err := exec.LookPath(command[0])
	if err != nil {
		return err
	}
	if dir != "" {
		if err = os.Chdir(dir); err != nil {
			return err
		}
	}
	return syscall.Exec(binary, command, os.Environ())
}
