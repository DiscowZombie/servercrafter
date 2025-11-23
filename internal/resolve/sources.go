package resolve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/discowzombie/servercrafter/internal/fetch"
	"github.com/discowzombie/servercrafter/internal/project"
	"github.com/discowzombie/servercrafter/internal/template"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/transport"
	githttp "github.com/go-git/go-git/v6/plumbing/transport/http"
	"github.com/go-git/go-git/v6/plumbing/transport/ssh"
	"github.com/sirupsen/logrus"
)

// source serves the files of a named project source by path
type source interface {
	fetchPath(ctx context.Context, p string) (*Resolved, error)
}

// mavenSource serves Maven artifacts by coordinates
type mavenSource interface {
	maven(ctx context.Context, d *template.Download) (*Resolved, error)
}

// localSource serves a directory of the project, for committed or locally built jars
type localSource struct {
	name string
	dir  string
}

func (l *localSource) fetchPath(_ context.Context, p string) (*Resolved, error) {
	full := filepath.Join(l.dir, filepath.FromSlash(path.Clean("/"+p)))
	info, err := os.Stat(full)
	if err != nil {
		return nil, fmt.Errorf("source %q: %w", l.name, err)
	}
	res := &Resolved{Origin: l.name + ":" + p}
	if info.IsDir() {
		res.Dir = full
		return res, nil
	}
	res.Open = func(context.Context) (io.ReadCloser, error) { return os.Open(full) }
	return res, nil
}

// httpSource serves files below a base URL, also read as a Maven repository. S3 sources are http
// sources whose requests are signed.
type httpSource struct {
	client *fetch.Client
	log    logrus.FieldLogger
	base   string
	auth   fetch.Auth
}

func (h *httpSource) fetchPath(_ context.Context, p string) (*Resolved, error) {
	u := joinURL(h.base, p)
	return &Resolved{Origin: u, Open: h.client.HTTPOpener(u, h.auth, nil)}, nil
}

func (h *httpSource) maven(ctx context.Context, d *template.Download) (*Resolved, error) {
	repo := &mavenRepo{client: h.client, log: h.log, base: h.base, auth: h.auth}
	return repo.resolve(ctx, d)
}

// gitlabSource reads the package registries of a GitLab instance
type gitlabSource struct {
	client  *fetch.Client
	log     logrus.FieldLogger
	api     string
	project string
	group   string
	auth    fetch.Auth
}

func (g *gitlabSource) fetchPath(context.Context, string) (*Resolved, error) {
	return nil, errors.New("a gitlab source serves packages: use a maven or gitlab-generic download")
}

func (g *gitlabSource) maven(ctx context.Context, d *template.Download) (*Resolved, error) {
	// What the artifact declares wins over the source defaults, whichever of the two it declares
	projectID, group := d.Project, d.Group
	if projectID == "" && group == "" {
		projectID, group = g.project, g.group
	}
	var scope string
	switch {
	case projectID != "":
		scope = "projects/" + url.PathEscape(projectID) + "/packages/maven"
	case group != "":
		scope = "groups/" + url.PathEscape(group) + "/-/packages/maven"
	default:
		// Instance level endpoint, only served by GitLab when the top level group of the package
		// matches the first segment of its group id
		scope = "packages/maven"
	}
	repo := &mavenRepo{client: g.client, log: g.log, base: g.api + "/" + scope, auth: g.auth}
	return repo.resolve(ctx, d)
}

func (g *gitlabSource) generic(_ context.Context, d *template.Download) (*Resolved, error) {
	projectID := d.Project
	if projectID == "" {
		projectID = g.project
	}
	if projectID == "" {
		return nil, errors.New("no project: set it on the download or on the gitlab source")
	}
	u := fmt.Sprintf("%s/projects/%s/packages/generic/%s/%s/%s", g.api,
		url.PathEscape(projectID), url.PathEscape(d.Package), url.PathEscape(d.Version), url.PathEscape(d.File))
	return &Resolved{Origin: u, Version: d.Version, Open: g.client.HTTPOpener(u, g.auth, nil)}, nil
}

// gitSource serves the files of a repository, cloned once per build
type gitSource struct {
	log        logrus.FieldLogger
	repository string
	ref        string
	auth       transport.AuthMethod

	once   sync.Once
	dir    string
	commit string
	err    error
}

func (g *gitSource) fetchPath(ctx context.Context, p string) (*Resolved, error) {
	g.once.Do(func() { g.err = g.clone(ctx) })
	if g.err != nil {
		return nil, g.err
	}
	full := filepath.Join(g.dir, filepath.FromSlash(path.Clean("/"+p)))
	info, err := os.Stat(full)
	if err != nil {
		return nil, fmt.Errorf("%s at %s: %w", p, g.commit, err)
	}
	res := &Resolved{Origin: g.origin() + "#" + p, Version: g.commit}
	if info.IsDir() {
		res.Dir = full
		return res, nil
	}
	res.Open = func(context.Context) (io.ReadCloser, error) { return os.Open(full) }
	return res, nil
}

func (g *gitSource) origin() string {
	origin := g.repository
	if u, err := url.Parse(g.repository); err == nil && u.User != nil {
		u.User = nil
		origin = u.String()
	}
	if g.ref != "" {
		origin += "@" + g.ref
	}
	return origin
}

func (g *gitSource) clone(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "servercrafter-git-*")
	if err != nil {
		return err
	}
	g.dir = dir
	var refs []plumbing.ReferenceName
	if g.ref == "" {
		refs = []plumbing.ReferenceName{""}
	} else {
		refs = []plumbing.ReferenceName{plumbing.NewBranchReferenceName(g.ref), plumbing.NewTagReferenceName(g.ref)}
	}
	g.log.WithField("repository", g.origin()).Debug("Cloning repository")
	var repo *git.Repository
	for _, ref := range refs {
		repo, err = git.PlainCloneContext(ctx, dir, &git.CloneOptions{
			URL:           g.repository,
			Auth:          g.auth,
			ReferenceName: ref,
			SingleBranch:  ref != "",
			Depth:         1,
		})
		if err == nil {
			break
		}
		// A failed attempt may leave a partial repository behind
		_ = os.RemoveAll(dir)
		_ = os.MkdirAll(dir, 0o700)
	}
	if err != nil {
		return fmt.Errorf("clone %s: %w", g.origin(), err)
	}
	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("clone %s: %w", g.origin(), err)
	}
	g.commit = head.Hash().String()[:12]
	g.log.WithField("repository", g.origin()).Debugf("Cloned at %s", g.commit)
	return nil
}

func (g *gitSource) close() error {
	if g.dir == "" {
		return nil
	}
	return os.RemoveAll(g.dir)
}

func (r *Resolver) gitAuth(name string) (transport.AuthMethod, error) {
	if name == "" {
		return nil, nil
	}
	cred, ok := r.project.Credentials[name]
	if !ok {
		return nil, fmt.Errorf("unknown credential %q", name)
	}
	read := func(field string, v *project.Value) (string, error) {
		value, err := v.Read(r.project.Dir)
		if err != nil {
			return "", fmt.Errorf("credential %q: %s: %w", name, field, err)
		}
		return value, nil
	}
	switch cred.Type {
	case project.CredentialBasic:
		user, err := read("username", cred.Username)
		if err != nil {
			return nil, err
		}
		password, err := read("password", cred.Password)
		if err != nil {
			return nil, err
		}
		return &githttp.BasicAuth{Username: user, Password: password}, nil
	case project.CredentialSSH:
		key, err := read("privateKey", cred.PrivateKey)
		if err != nil {
			return nil, err
		}
		var passphrase string
		if cred.Passphrase != nil {
			if passphrase, err = read("passphrase", cred.Passphrase); err != nil {
				return nil, err
			}
		}
		user := cred.User
		if user == "" {
			user = "git"
		}
		auth, err := ssh.NewPublicKeys(user, []byte(strings.TrimSpace(key)+"\n"), passphrase)
		if err != nil {
			return nil, fmt.Errorf("credential %q: %w", name, err)
		}
		return auth, nil
	}
	return nil, fmt.Errorf("credential %q: a %s credential cannot authenticate git", name, cred.Type)
}
