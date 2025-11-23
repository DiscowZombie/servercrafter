package build

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/discowzombie/servercrafter/internal/fetch"
	"github.com/discowzombie/servercrafter/internal/manifest"
	"golang.org/x/sync/errgroup"
)

// Change statuses
const (
	StatusAdd       = "add"
	StatusUpdate    = "update"
	StatusRemove    = "remove"
	StatusModified  = "modified"
	StatusUnchanged = "unchanged"
	StatusUnknown   = "unknown"
)

// Change is the difference between what a template resolves to and what a server holds
type Change struct {
	ID     string
	Path   string
	Status string
	From   string
	To     string
}

// Diff resolves the template without downloading anything and compares it with the server built
// in opts.OutDir
func Diff(ctx context.Context, opts Options) ([]Change, error) {
	t := opts.Template
	if err := t.ValidateBuildable(); err != nil {
		return nil, err
	}
	if err := t.CheckProject(opts.Project); err != nil {
		return nil, err
	}
	items, err := Plan(t, opts.Overrides, opts.Env, opts.Log)
	if err != nil {
		return nil, err
	}
	previous, err := manifest.ReadIfExists(opts.OutDir)
	if err != nil {
		return nil, err
	}
	jobs := opts.Jobs
	if jobs < 1 {
		jobs = 4
	}
	changes := make([]Change, len(items))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(jobs)
	var mu sync.Mutex
	planned := map[string]bool{}
	for i := range items {
		item := items[i]
		mu.Lock()
		planned[item.ID] = true
		mu.Unlock()
		group.Go(func() error {
			change, err := diffItem(groupCtx, opts, item, previous)
			if err != nil {
				return fmt.Errorf("%s: %w", item.ID, err)
			}
			changes[i] = change
			return nil
		})
	}
	if err = group.Wait(); err != nil {
		return nil, err
	}
	if previous != nil {
		for _, prev := range previous.Entries {
			if !planned[prev.ID] {
				changes = append(changes, Change{ID: prev.ID, Path: prev.Path, Status: StatusRemove, From: prev.Version})
			}
		}
	}
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].ID < changes[j].ID })
	return changes, nil
}

func diffItem(ctx context.Context, opts Options, item Item, previous *manifest.Manifest) (Change, error) {
	change := Change{ID: item.ID, Path: item.Path}
	prev, hasPrev := previous.Find(item.ID)
	if hasPrev {
		change.From = prev.Version
	}
	target := filepath.Join(opts.OutDir, filepath.FromSlash(item.Path))
	_, statErr := os.Stat(target)
	exists := statErr == nil

	if item.World != nil && exists && !item.World.Overwrite {
		change.Status = StatusUnchanged
		change.To = change.From
		return change, nil
	}
	res, err := opts.Resolver.Resolve(ctx, item.Artifact)
	if err != nil {
		return change, err
	}
	change.To = res.Version
	switch {
	case !exists:
		change.Status = StatusAdd
	case item.World != nil:
		change.Status = StatusUpdate
		if hasPrev && prev.Version != "" && prev.Version == res.Version && prev.Origin == res.Origin {
			change.Status = StatusUnchanged
		}
	default:
		sum, err := fetch.FileSHA256(target)
		if err != nil {
			return change, err
		}
		switch {
		case hasPrev && prev.SHA256 != "" && prev.SHA256 != sum:
			change.Status = StatusModified
		case len(res.Checksums) > 0:
			if _, ok := upToDate(target, res, previous, item); ok {
				change.Status = StatusUnchanged
			} else {
				change.Status = StatusUpdate
			}
		case res.Version != "" && hasPrev:
			if prev.Version == res.Version && prev.Origin == res.Origin {
				change.Status = StatusUnchanged
			} else {
				change.Status = StatusUpdate
			}
		default:
			// Nothing tells which content the source currently serves
			change.Status = StatusUnknown
		}
	}
	return change, nil
}
