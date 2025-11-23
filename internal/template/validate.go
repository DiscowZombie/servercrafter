package template

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/discowzombie/servercrafter/internal/project"
)

var (
	namePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	jarIDPattern  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	sha256Pattern = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
)

// PatchableExtensions are the configuration formats patches can edit
var PatchableExtensions = []string{".properties", ".yml", ".yaml", ".json"}

// Validate checks the structure of a single template file, before its parents are merged
func (t *Template) Validate() error {
	var errs []error
	add := func(prefix string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", prefix, err))
		}
	}
	if t.Version != 1 {
		add("version", fmt.Errorf("must be 1, got %d", t.Version))
	}
	if t.ID != "" && !namePattern.MatchString(t.ID) {
		add("id", fmt.Errorf("%q must match %s", t.ID, namePattern))
	}
	for i, parent := range t.Parents {
		if !namePattern.MatchString(parent) {
			add(fmt.Sprintf("parents[%d]", i), fmt.Errorf("%q must match %s", parent, namePattern))
		}
	}
	if t.Server != nil {
		if t.Server.Name != "" {
			add("server.name", validateFileName(t.Server.Name, ".jar"))
		}
		add("server", t.Server.validateLocation())
	}
	jarIDs := map[string]bool{}
	for i, plugin := range t.Plugins {
		prefix := fmt.Sprintf("plugins[%d]", i)
		if plugin.JarID != "" {
			prefix = fmt.Sprintf("plugins[%s]", plugin.JarID)
		}
		add(prefix, plugin.validate())
		if jarIDs[plugin.JarID] {
			add(prefix, errors.New("jarId declared twice in the same template"))
		}
		jarIDs[plugin.JarID] = true
	}
	worlds := map[string]bool{}
	for i, world := range t.Worlds {
		prefix := fmt.Sprintf("worlds[%d]", i)
		if world.Name != "" {
			prefix = fmt.Sprintf("worlds[%s]", world.Name)
		}
		add(prefix, world.validate())
		if worlds[world.Name] {
			add(prefix, errors.New("world declared twice in the same template"))
		}
		worlds[world.Name] = true
	}
	for i, file := range t.Files {
		add(fmt.Sprintf("files[%d]", i), validateRelativePath(file, true))
	}
	for i, patch := range t.Patches {
		add(fmt.Sprintf("patches[%d]", i), patch.validate())
	}
	for i, patch := range t.RuntimePatches {
		add(fmt.Sprintf("runtimePatches[%d]", i), patch.validate())
	}
	return errors.Join(errs...)
}

// ValidateBuildable checks a flattened template has everything a build needs
func (t *Template) ValidateBuildable() error {
	if t.Server == nil {
		return errors.New("server: no template of the chain declares the server jar")
	}
	return nil
}

// CheckProject checks every source, credential and overlay the template references exists in the
// project and fits the way it is used
func (t *Template) CheckProject(p *project.Project) error {
	var errs []error
	check := func(prefix string, a *Artifact) {
		if a == nil {
			return
		}
		if err := a.checkProject(p); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", prefix, err))
		}
	}
	check("server", t.Server)
	for _, plugin := range t.Plugins {
		check("plugins["+plugin.JarID+"]", &plugin.Artifact)
	}
	for _, world := range t.Worlds {
		check("worlds["+world.Name+"]", &world.Artifact)
	}
	for _, dir := range t.Files {
		info, err := os.Stat(p.Resolve(dir))
		if err != nil {
			errs = append(errs, fmt.Errorf("files: %w", err))
		} else if !info.IsDir() {
			errs = append(errs, fmt.Errorf("files: %s is not a directory", dir))
		}
	}
	return errors.Join(errs...)
}

func (p Plugin) validate() error {
	if !jarIDPattern.MatchString(p.JarID) {
		return fmt.Errorf("jarId %q must match %s", p.JarID, jarIDPattern)
	}
	if p.Remove {
		if p.Artifact != (Artifact{}) {
			return errors.New("remove: true takes no other field than jarId")
		}
		return nil
	}
	var errs []error
	if err := validateFileName(p.Name, ".jar"); err != nil {
		errs = append(errs, fmt.Errorf("name: %w", err))
	}
	errs = append(errs, p.validateLocation())
	return errors.Join(errs...)
}

func (w World) validate() error {
	if err := validateFileName(w.Name, ""); err != nil {
		return fmt.Errorf("name: %w", err)
	}
	if w.Remove {
		if w.Artifact != (Artifact{Name: w.Name}) || w.Extract != "" || w.StripComponents != 0 || w.Overwrite {
			return errors.New("remove: true takes no other field than name")
		}
		return nil
	}
	var errs []error
	switch w.Extract {
	case ExtractNone:
		if w.StripComponents != 0 {
			errs = append(errs, errors.New("stripComponents requires extract"))
		}
	case ExtractZip:
	default:
		errs = append(errs, fmt.Errorf("extract: unsupported %q (zip)", w.Extract))
	}
	if w.StripComponents < 0 {
		errs = append(errs, errors.New("stripComponents must be positive"))
	}
	errs = append(errs, w.validateLocation())
	return errors.Join(errs...)
}

// validateLocation checks exactly one way of locating the artifact is used
func (a *Artifact) validateLocation() error {
	var errs []error
	forms := 0
	if a.Download != nil {
		forms++
		if err := a.Download.validate(); err != nil {
			errs = append(errs, fmt.Errorf("download: %w", err))
		}
	}
	if a.URL != "" {
		forms++
		if err := project.ValidateURL(a.URL, false); err != nil {
			errs = append(errs, fmt.Errorf("url: %w", err))
		}
	}
	if a.Source != "" || a.Path != "" {
		forms++
		if a.Source == "" || a.Path == "" {
			errs = append(errs, errors.New("source and path go together"))
		} else if err := validateRelativePath(a.Path, false); err != nil {
			errs = append(errs, fmt.Errorf("path: %w", err))
		}
	}
	if forms != 1 {
		errs = append(errs, errors.New("exactly one of download, url or source+path is required"))
	}
	if a.SHA256 != "" && !sha256Pattern.MatchString(a.SHA256) {
		errs = append(errs, errors.New("sha256: must be 64 hexadecimal characters"))
	}
	if a.Destination != "" {
		if err := validateRelativePath(a.Destination, false); err != nil {
			errs = append(errs, fmt.Errorf("destination: %w", err))
		}
	}
	return errors.Join(errs...)
}

type downloadRule struct {
	required []string
	optional []string
}

var downloadRules = map[string]downloadRule{
	DownloadMaven: {
		required: []string{"groupId", "artifactId", "version"},
		optional: []string{"source", "repository", "credentials", "classifier", "extension", "project", "group"},
	},
	DownloadPaperMC:       {required: []string{"project"}, optional: []string{"version", "build", "channel"}},
	DownloadModrinth:      {required: []string{"project"}, optional: []string{"version", "loaders", "gameVersions", "asset"}},
	DownloadGithub:        {required: []string{"repository", "asset"}, optional: []string{"version", "credentials"}},
	DownloadLuckPerms:     {required: []string{"platform"}, optional: []string{"version"}},
	DownloadGitlabGeneric: {required: []string{"source", "package", "version", "file"}, optional: []string{"project"}},
}

func (d *Download) fields() map[string]bool {
	return map[string]bool{
		"version":      d.Version != "",
		"source":       d.Source != "",
		"repository":   d.Repository != "",
		"credentials":  d.Credentials != "",
		"groupId":      d.GroupID != "",
		"artifactId":   d.ArtifactID != "",
		"classifier":   d.Classifier != "",
		"extension":    d.Extension != "",
		"project":      d.Project != "",
		"group":        d.Group != "",
		"build":        d.Build != 0,
		"channel":      d.Channel != "",
		"loaders":      len(d.Loaders) > 0,
		"gameVersions": len(d.GameVersions) > 0,
		"asset":        d.Asset != "",
		"platform":     d.Platform != "",
		"package":      d.Package != "",
		"file":         d.File != "",
	}
}

func (d *Download) validate() error {
	rule, ok := downloadRules[d.Type]
	if !ok {
		return fmt.Errorf("unknown type %q (maven, papermc, modrinth, github, luckperms or gitlab-generic)", d.Type)
	}
	var errs []error
	set := d.fields()
	allowed := map[string]bool{}
	for _, field := range rule.required {
		allowed[field] = true
		if !set[field] {
			errs = append(errs, fmt.Errorf("%s is required for a %s download", field, d.Type))
		}
	}
	for _, field := range rule.optional {
		allowed[field] = true
	}
	for _, field := range sortedFieldNames(set) {
		if set[field] && !allowed[field] {
			errs = append(errs, fmt.Errorf("%s is not supported by a %s download", field, d.Type))
		}
	}
	switch d.Type {
	case DownloadMaven:
		if d.Source == "" && d.Repository == "" {
			errs = append(errs, errors.New("source or repository (the Maven repository URL) is required"))
		}
		if d.Source == "" && d.Repository != "" {
			if err := project.ValidateURL(d.Repository, d.Credentials != ""); err != nil {
				errs = append(errs, fmt.Errorf("repository: %w", err))
			}
		}
		if d.Source != "" && d.Credentials != "" {
			errs = append(errs, errors.New("credentials only apply with a repository URL, a source brings its own"))
		}
		if d.Project != "" && d.Group != "" {
			errs = append(errs, errors.New("project and group are mutually exclusive"))
		}
	case DownloadPaperMC:
		switch strings.ToLower(d.Channel) {
		case "", "stable", "beta", "alpha":
		default:
			errs = append(errs, fmt.Errorf("channel: unsupported %q (stable, beta or alpha)", d.Channel))
		}
		if d.Build != 0 && (d.Version == "" || d.Version == "latest") {
			errs = append(errs, errors.New("build requires an explicit version"))
		}
	case DownloadGithub:
		if strings.Count(d.Repository, "/") != 1 {
			errs = append(errs, fmt.Errorf("repository: %q must be owner/name", d.Repository))
		}
	case DownloadLuckPerms:
		if d.Version != "" && d.Version != "latest" {
			errs = append(errs, errors.New("version: LuckPerms only publishes its latest build, use latest"))
		}
	}
	if d.Asset != "" {
		if _, err := regexp.Compile(d.Asset); err != nil {
			errs = append(errs, fmt.Errorf("asset: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (a *Artifact) checkProject(p *project.Project) error {
	source := func(name string, allowed ...string) error {
		s, ok := p.Sources[name]
		if !ok {
			return fmt.Errorf("unknown source %q", name)
		}
		for _, t := range allowed {
			if s.Type == t {
				return nil
			}
		}
		return fmt.Errorf("source %q is a %s source, expected %s", name, s.Type, strings.Join(allowed, " or "))
	}
	switch {
	case a.Source != "":
		return source(a.Source, project.SourceLocal, project.SourceHTTP, project.SourceS3, project.SourceNexus, project.SourceGit)
	case a.Download == nil:
		return nil
	}
	d := a.Download
	var errs []error
	if d.Credentials != "" {
		cred, ok := p.Credentials[d.Credentials]
		if !ok {
			errs = append(errs, fmt.Errorf("download.credentials: unknown credential %q", d.Credentials))
		} else if cred.Type != project.CredentialBasic && cred.Type != project.CredentialHeader &&
			cred.Type != project.CredentialBearer {
			errs = append(errs, fmt.Errorf("download.credentials: a %s credential cannot authenticate http requests", cred.Type))
		}
	}
	switch d.Type {
	case DownloadMaven:
		if d.Source != "" {
			if err := source(d.Source, project.SourceNexus, project.SourceGitlab, project.SourceHTTP, project.SourceS3); err != nil {
				errs = append(errs, fmt.Errorf("download.source: %w", err))
			} else {
				kind := p.Sources[d.Source].Type
				if d.Repository != "" && kind != project.SourceNexus {
					errs = append(errs, errors.New("download.repository: only overrides the repository of a nexus source"))
				}
				if (d.Project != "" || d.Group != "") && kind != project.SourceGitlab {
					errs = append(errs, errors.New("download.project/group: only apply to a gitlab source"))
				}
			}
		}
	case DownloadGitlabGeneric:
		if err := source(d.Source, project.SourceGitlab); err != nil {
			errs = append(errs, fmt.Errorf("download.source: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (p Patch) validate() error {
	var errs []error
	if err := validateRelativePath(p.File, false); err != nil {
		errs = append(errs, fmt.Errorf("file: %w", err))
	} else if !hasPatchableExtension(p.File) {
		errs = append(errs, fmt.Errorf("file: %s cannot be patched (%s)", p.File, strings.Join(PatchableExtensions, ", ")))
	}
	if len(p.Fields) == 0 {
		errs = append(errs, errors.New("fields: at least one field is required"))
	}
	for i, field := range p.Fields {
		if field.Field == "" {
			errs = append(errs, fmt.Errorf("fields[%d]: field is required", i))
		}
		switch field.Value.(type) {
		case string, int, int64, float64, bool:
		default:
			errs = append(errs, fmt.Errorf("fields[%d] (%s): value must be a string, a number or a boolean", i, field.Field))
		}
		switch field.Type {
		case "", "string", "int", "float", "bool":
		default:
			errs = append(errs, fmt.Errorf("fields[%d] (%s): unsupported type %q (string, int, float or bool)", i, field.Field, field.Type))
		}
	}
	return errors.Join(errs...)
}

func hasPatchableExtension(file string) bool {
	ext := strings.ToLower(path.Ext(file))
	for _, e := range PatchableExtensions {
		if ext == e {
			return true
		}
	}
	return false
}

// validateFileName checks name is a single path element, with the given extension when not empty
func validateFileName(name string, ext string) error {
	if name == "" {
		return errors.New("is required")
	}
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return fmt.Errorf("%q must be a file name, not a path", name)
	}
	if ext != "" && !strings.HasSuffix(name, ext) {
		return fmt.Errorf("%q must end with %s", name, ext)
	}
	return nil
}

// validateRelativePath rejects absolute paths and paths escaping their root. Overlays (files) are
// resolved from the project, so they may climb out of it when allowParent is set.
func validateRelativePath(p string, allowParent bool) error {
	if p == "" {
		return errors.New("is required")
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return fmt.Errorf("%q must be relative", p)
	}
	if !allowParent {
		clean := path.Clean(filepath.ToSlash(p))
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("%q must not leave its root directory", p)
		}
	}
	return nil
}

func sortedFieldNames(m map[string]bool) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
