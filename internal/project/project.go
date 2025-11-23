// Package project loads servercrafter.yaml: where templates live, the named sources artifacts are
// fetched from and the credentials those sources authenticate with.
package project

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/discowzombie/servercrafter/internal/yamlutil"
)

// DefaultFile is the project file looked up in the working directory when none is given
const DefaultFile = "servercrafter.yaml"

// Source types
const (
	SourceLocal  = "local"
	SourceHTTP   = "http"
	SourceNexus  = "nexus"
	SourceGitlab = "gitlab"
	SourceS3     = "s3"
	SourceGit    = "git"
)

// Credential types
const (
	CredentialBasic  = "basic"
	CredentialHeader = "header"
	CredentialBearer = "bearer"
	CredentialSSH    = "ssh"
	CredentialS3     = "s3"
)

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// Project is the content of servercrafter.yaml
type Project struct {
	Schema       string                 `yaml:"$schema,omitempty"`
	Version      int                    `yaml:"version"`
	TemplatesDir string                 `yaml:"templatesDir,omitempty"`
	Credentials  map[string]*Credential `yaml:"credentials,omitempty"`
	Sources      map[string]*Source     `yaml:"sources,omitempty"`

	// Dir is the directory every relative path of the project and its templates is resolved from
	Dir string `yaml:"-"`
	// Path of the file the project was loaded from, empty for an implicit project
	Path string `yaml:"-"`
}

// Source is a named place artifacts are downloaded from
type Source struct {
	Type string `yaml:"type"`
	// local
	Directory string `yaml:"directory,omitempty"`
	// http, nexus, gitlab, s3
	BaseURL string `yaml:"baseUrl,omitempty"`
	// nexus: default repository name; git: repository URL
	Repository string `yaml:"repository,omitempty"`
	// gitlab: default project (ID or full path) or group the Maven registry is read from
	Project string `yaml:"project,omitempty"`
	Group   string `yaml:"group,omitempty"`
	// git: branch or tag to check out, the remote HEAD when empty
	Ref string `yaml:"ref,omitempty"`
	// Name of the credential the source authenticates with
	Credentials string `yaml:"credentials,omitempty"`
}

// Credential holds how to read a secret, never the secret itself
type Credential struct {
	Type string `yaml:"type"`
	// basic (also git over http)
	Username *Value `yaml:"username,omitempty"`
	Password *Value `yaml:"password,omitempty"`
	// header
	Name  string `yaml:"name,omitempty"`
	Value *Value `yaml:"value,omitempty"`
	// bearer
	Token *Value `yaml:"token,omitempty"`
	// ssh
	User       string `yaml:"user,omitempty"`
	PrivateKey *Value `yaml:"privateKey,omitempty"`
	Passphrase *Value `yaml:"passphrase,omitempty"`
	// s3
	AccessKey *Value `yaml:"accessKey,omitempty"`
	SecretKey *Value `yaml:"secretKey,omitempty"`
	Region    string `yaml:"region,omitempty"`
}

// Value points at a secret: an environment variable or a file (Docker/Kubernetes secrets). Fields
// that are not secrets, such as a basic username, may also be written in the project as a value.
type Value struct {
	Env   string `yaml:"env,omitempty"`
	File  string `yaml:"file,omitempty"`
	Plain string `yaml:"value,omitempty"`
}

// Implicit returns the project used when no project file exists: no sources, templates and
// overlays resolved from dir
func Implicit(dir string) *Project {
	return &Project{Version: 1, Dir: dir, TemplatesDir: "."}
}

// Load reads and validates a project file
func Load(path string) (*Project, error) {
	p := &Project{}
	if err := yamlutil.DecodeFile(path, p); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	p.Path = abs
	p.Dir = filepath.Dir(abs)
	if p.TemplatesDir == "" {
		p.TemplatesDir = "templates"
	}
	if err = p.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// LoadOrImplicit loads path when it exists. A missing file is an error only when explicit is set,
// otherwise the implicit project rooted at the working directory is returned.
func LoadOrImplicit(path string, explicit bool) (*Project, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) && !explicit {
			wd, err := os.Getwd()
			if err != nil {
				return nil, err
			}
			return Implicit(wd), nil
		}
		return nil, err
	}
	return Load(path)
}

// TemplatesPath is the absolute directory holding the templates
func (p *Project) TemplatesPath() string {
	return p.Resolve(p.TemplatesDir)
}

// Resolve makes a project relative path absolute
func (p *Project) Resolve(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(p.Dir, path)
}

// Validate checks the project is self-consistent: every source is well-formed and references an
// existing credential of a fitting type
func (p *Project) Validate() error {
	var errs []error
	if p.Version != 1 {
		errs = append(errs, fmt.Errorf("version: must be 1, got %d", p.Version))
	}
	for _, name := range sortedKeys(p.Credentials) {
		if err := invalidName("credentials", name); err != nil {
			errs = append(errs, err)
		}
		if err := p.Credentials[name].validate(); err != nil {
			errs = append(errs, fmt.Errorf("credentials.%s: %w", name, err))
		}
	}
	for _, name := range sortedKeys(p.Sources) {
		if err := invalidName("sources", name); err != nil {
			errs = append(errs, err)
		}
		if err := p.validateSource(p.Sources[name]); err != nil {
			errs = append(errs, fmt.Errorf("sources.%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func invalidName(kind string, name string) error {
	if namePattern.MatchString(name) {
		return nil
	}
	return fmt.Errorf("%s: invalid name %q (letters, digits, '-' and '_' only)", kind, name)
}

func (p *Project) validateSource(s *Source) error {
	if s == nil {
		return errors.New("empty source")
	}
	var errs []error
	req := func(field string, value string) {
		if value == "" {
			errs = append(errs, fmt.Errorf("%s is required for a %s source", field, s.Type))
		}
	}
	forbid := func(field string, value string) {
		if value != "" {
			errs = append(errs, fmt.Errorf("%s is not supported by a %s source", field, s.Type))
		}
	}
	var allowedCredentials []string
	switch s.Type {
	case SourceLocal:
		req("directory", s.Directory)
		forbid("baseUrl", s.BaseURL)
		forbid("repository", s.Repository)
		forbid("credentials", s.Credentials)
	case SourceHTTP, SourceS3, SourceNexus, SourceGitlab:
		req("baseUrl", s.BaseURL)
		forbid("directory", s.Directory)
		forbid("ref", s.Ref)
		if s.Type != SourceNexus {
			forbid("repository", s.Repository)
		}
		if s.Type != SourceGitlab {
			forbid("project", s.Project)
			forbid("group", s.Group)
		} else if s.Project != "" && s.Group != "" {
			errs = append(errs, errors.New("project and group are mutually exclusive"))
		}
		if s.BaseURL != "" {
			if err := ValidateURL(s.BaseURL, s.Credentials != ""); err != nil {
				errs = append(errs, fmt.Errorf("baseUrl: %w", err))
			}
		}
		switch s.Type {
		case SourceS3:
			allowedCredentials = []string{CredentialS3}
		default:
			allowedCredentials = []string{CredentialBasic, CredentialHeader, CredentialBearer}
		}
	case SourceGit:
		req("repository", s.Repository)
		forbid("directory", s.Directory)
		forbid("baseUrl", s.BaseURL)
		allowedCredentials = []string{CredentialBasic, CredentialSSH}
	case "":
		return errors.New("type is required (local, http, nexus, gitlab, s3 or git)")
	default:
		return fmt.Errorf("unknown type %q (local, http, nexus, gitlab, s3 or git)", s.Type)
	}
	if s.Credentials != "" {
		cred, ok := p.Credentials[s.Credentials]
		if !ok {
			errs = append(errs, fmt.Errorf("credentials: unknown credential %q", s.Credentials))
		} else if !contains(allowedCredentials, cred.Type) {
			errs = append(errs, fmt.Errorf("credentials: a %s source cannot use a %s credential (%s)",
				s.Type, cred.Type, strings.Join(allowedCredentials, ", ")))
		}
	}
	return errors.Join(errs...)
}

// ValidateURL checks an http(s) URL carries no userinfo and, when it is sent credentials, that the
// transport is encrypted
func ValidateURL(raw string, withCredentials bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%q: missing host", raw)
	}
	if u.User != nil {
		return fmt.Errorf("%q: credentials must not be embedded in URLs, use a named credential", redact(u))
	}
	if withCredentials && u.Scheme != "https" && !isLoopback(u.Hostname()) {
		return fmt.Errorf("%q: credentials are only sent over https", raw)
	}
	return nil
}

func redact(u *url.URL) string {
	c := *u
	c.User = url.User("***")
	return c.String()
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func (c *Credential) validate() error {
	if c == nil {
		return errors.New("empty credential")
	}
	var errs []error
	value := func(field string, v *Value, required bool) {
		if v == nil {
			if required {
				errs = append(errs, fmt.Errorf("%s is required for a %s credential", field, c.Type))
			}
			return
		}
		// Only the username is not a secret: everything else must be read from outside the project
		if v.Plain != "" && !(c.Type == CredentialBasic && field == "username") {
			errs = append(errs, fmt.Errorf("%s: a secret cannot be written in the project, use env or file", field))
			return
		}
		if err := v.validate(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", field, err))
		}
	}
	forbid := func(field string, set bool) {
		if set {
			errs = append(errs, fmt.Errorf("%s is not supported by a %s credential", field, c.Type))
		}
	}
	switch c.Type {
	case CredentialBasic:
		value("username", c.Username, true)
		value("password", c.Password, true)
		forbid("value", c.Value != nil)
		forbid("token", c.Token != nil)
	case CredentialHeader:
		if c.Name == "" {
			errs = append(errs, errors.New("name (the header name) is required for a header credential"))
		}
		value("value", c.Value, true)
		forbid("username", c.Username != nil)
		forbid("token", c.Token != nil)
	case CredentialBearer:
		value("token", c.Token, true)
		forbid("value", c.Value != nil)
	case CredentialSSH:
		value("privateKey", c.PrivateKey, true)
		value("passphrase", c.Passphrase, false)
		forbid("password", c.Password != nil)
	case CredentialS3:
		value("accessKey", c.AccessKey, true)
		value("secretKey", c.SecretKey, true)
		if c.Region == "" {
			errs = append(errs, errors.New("region is required for an s3 credential"))
		}
	case "":
		return errors.New("type is required (basic, header, bearer, ssh or s3)")
	default:
		return fmt.Errorf("unknown type %q (basic, header, bearer, ssh or s3)", c.Type)
	}
	return errors.Join(errs...)
}

func (v *Value) validate() error {
	set := 0
	for _, s := range []string{v.Env, v.File, v.Plain} {
		if s != "" {
			set++
		}
	}
	if set != 1 {
		return errors.New("exactly one of env, file or value must be set")
	}
	return nil
}

// Read returns the secret. Files are resolved from baseDir and their trailing newline is dropped.
func (v *Value) Read(baseDir string) (string, error) {
	if v == nil {
		return "", nil
	}
	if v.Plain != "" {
		return v.Plain, nil
	}
	if v.Env != "" {
		value, ok := os.LookupEnv(v.Env)
		if !ok || value == "" {
			return "", fmt.Errorf("environment variable %s is not set", v.Env)
		}
		return value, nil
	}
	path := v.File
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, path[2:])
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secret file: %w", err)
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func contains(values []string, v string) bool {
	for _, value := range values {
		if value == v {
			return true
		}
	}
	return false
}
