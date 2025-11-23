// Package resolve turns the artifacts a template declares into something to install: where it
// comes from, which version that is, the checksums it must match and how to read it.
package resolve

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/discowzombie/servercrafter/internal/fetch"
	"github.com/discowzombie/servercrafter/internal/project"
	"github.com/discowzombie/servercrafter/internal/template"
	"github.com/sirupsen/logrus"
)

// Resolved is an artifact ready to be installed
type Resolved struct {
	// Origin tells where the artifact comes from (a URL, a source path), never with credentials
	Origin string
	// Version the artifact resolved to, empty when it has no notion of one
	Version string
	// Checksums the content must match
	Checksums fetch.Checksums
	// Open reads the content of a file artifact
	Open fetch.Opener
	// Dir is set instead of Open for an artifact that is a directory
	Dir string
}

// APIs are the endpoints of the public services typed downloads query, overridable for tests
type APIs struct {
	PaperMC   string
	Modrinth  string
	Github    string
	LuckPerms string
}

// DefaultAPIs are the public endpoints
var DefaultAPIs = APIs{
	PaperMC:   "https://fill.papermc.io/v3",
	Modrinth:  "https://api.modrinth.com/v2",
	Github:    "https://api.github.com",
	LuckPerms: "https://metadata.luckperms.net/data/all",
}

// Resolver resolves artifacts against the sources and credentials of a project. Sources and
// credentials are set up on first use, so a missing secret only fails the builds that need it.
type Resolver struct {
	project *project.Project
	http    *fetch.Client
	log     logrus.FieldLogger
	APIs    APIs

	mu      sync.Mutex
	sources map[string]source
	git     []*gitSource
}

// New returns a resolver of the artifacts of p
func New(p *project.Project, client *fetch.Client, log logrus.FieldLogger) *Resolver {
	return &Resolver{
		project: p,
		http:    client,
		log:     log,
		APIs:    DefaultAPIs,
		sources: map[string]source{},
	}
}

// Close releases what sources hold, such as cloned repositories
func (r *Resolver) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []error
	for _, g := range r.git {
		errs = append(errs, g.close())
	}
	return errors.Join(errs...)
}

// Resolve finds what the artifact points at, without downloading it
func (r *Resolver) Resolve(ctx context.Context, a *template.Artifact) (*Resolved, error) {
	var res *Resolved
	var err error
	switch {
	case a.URL != "":
		res = &Resolved{Origin: a.URL, Open: r.http.HTTPOpener(a.URL, nil, nil)}
	case a.Source != "":
		var src source
		if src, err = r.source(a.Source); err == nil {
			res, err = src.fetchPath(ctx, a.Path)
		}
	case a.Download != nil:
		res, err = r.download(ctx, a.Download)
	default:
		err = errors.New("artifact has no location")
	}
	if err != nil {
		return nil, err
	}
	if res.Checksums == nil {
		res.Checksums = fetch.Checksums{}
	}
	if a.SHA256 != "" {
		if res.Dir != "" {
			return nil, errors.New("sha256 cannot verify a directory")
		}
		if err = res.Checksums.Add("sha256", a.SHA256); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func (r *Resolver) download(ctx context.Context, d *template.Download) (*Resolved, error) {
	switch d.Type {
	case template.DownloadMaven:
		if d.Source == "" {
			auth, err := r.httpAuth(d.Credentials)
			if err != nil {
				return nil, err
			}
			repo := &mavenRepo{client: r.http, log: r.log, base: d.Repository, auth: auth}
			return repo.resolve(ctx, d)
		}
		src, err := r.source(d.Source)
		if err != nil {
			return nil, err
		}
		maven, ok := src.(mavenSource)
		if !ok {
			return nil, fmt.Errorf("source %q cannot serve Maven artifacts", d.Source)
		}
		return maven.maven(ctx, d)
	case template.DownloadGitlabGeneric:
		src, err := r.source(d.Source)
		if err != nil {
			return nil, err
		}
		gitlab, ok := src.(*gitlabSource)
		if !ok {
			return nil, fmt.Errorf("source %q is not a gitlab source", d.Source)
		}
		return gitlab.generic(ctx, d)
	case template.DownloadPaperMC:
		return r.paperMC(ctx, d)
	case template.DownloadModrinth:
		return r.modrinth(ctx, d)
	case template.DownloadGithub:
		return r.github(ctx, d)
	case template.DownloadLuckPerms:
		return r.luckPerms(ctx, d)
	}
	return nil, fmt.Errorf("unknown download type %q", d.Type)
}

func (r *Resolver) source(name string) (source, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if src, ok := r.sources[name]; ok {
		return src, nil
	}
	conf, ok := r.project.Sources[name]
	if !ok {
		return nil, fmt.Errorf("unknown source %q", name)
	}
	src, err := r.newSource(name, conf)
	if err != nil {
		return nil, fmt.Errorf("source %q: %w", name, err)
	}
	r.sources[name] = src
	return src, nil
}

func (r *Resolver) newSource(name string, conf *project.Source) (source, error) {
	switch conf.Type {
	case project.SourceLocal:
		return &localSource{name: name, dir: r.project.Resolve(conf.Directory)}, nil
	case project.SourceHTTP, project.SourceS3:
		auth, err := r.httpAuthUnlocked(conf.Credentials)
		if err != nil {
			return nil, err
		}
		return &httpSource{client: r.http, log: r.log, base: conf.BaseURL, auth: auth}, nil
	case project.SourceNexus:
		auth, err := r.httpAuthUnlocked(conf.Credentials)
		if err != nil {
			return nil, err
		}
		return &nexusSource{client: r.http, log: r.log, base: strings.TrimSuffix(conf.BaseURL, "/"),
			repository: conf.Repository, auth: auth}, nil
	case project.SourceGitlab:
		auth, err := r.httpAuthUnlocked(conf.Credentials)
		if err != nil {
			return nil, err
		}
		return &gitlabSource{client: r.http, log: r.log, api: strings.TrimSuffix(conf.BaseURL, "/") + "/api/v4",
			project: conf.Project, group: conf.Group, auth: auth}, nil
	case project.SourceGit:
		auth, err := r.gitAuth(conf.Credentials)
		if err != nil {
			return nil, err
		}
		g := &gitSource{log: r.log, repository: conf.Repository, ref: conf.Ref, auth: auth}
		r.git = append(r.git, g)
		return g, nil
	}
	return nil, fmt.Errorf("unknown source type %q", conf.Type)
}

func (r *Resolver) httpAuth(name string) (fetch.Auth, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.httpAuthUnlocked(name)
}

func (r *Resolver) httpAuthUnlocked(name string) (fetch.Auth, error) {
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
		return fetch.BasicAuth{Username: user, Password: password}, nil
	case project.CredentialHeader:
		value, err := read("value", cred.Value)
		if err != nil {
			return nil, err
		}
		return fetch.HeaderAuth{Name: cred.Name, Value: value}, nil
	case project.CredentialBearer:
		token, err := read("token", cred.Token)
		if err != nil {
			return nil, err
		}
		return fetch.HeaderAuth{Name: "Authorization", Value: "Bearer " + token}, nil
	case project.CredentialS3:
		accessKey, err := read("accessKey", cred.AccessKey)
		if err != nil {
			return nil, err
		}
		secretKey, err := read("secretKey", cred.SecretKey)
		if err != nil {
			return nil, err
		}
		return newS3Auth(accessKey, secretKey, cred.Region), nil
	}
	return nil, fmt.Errorf("credential %q: a %s credential cannot authenticate http requests", name, cred.Type)
}

// joinURL appends a slash separated path to a base URL, escaping each segment
func joinURL(base string, path string) string {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.Join(segments, "/")
}
