// Package template defines the servercrafter template format, loads templates and flattens their
// parents into a single buildable template.
package template

// Download types
const (
	DownloadMaven         = "maven"
	DownloadPaperMC       = "papermc"
	DownloadModrinth      = "modrinth"
	DownloadGithub        = "github"
	DownloadLuckPerms     = "luckperms"
	DownloadGitlabGeneric = "gitlab-generic"
)

// Extraction modes of a world
const (
	ExtractNone = ""
	ExtractZip  = "zip"
)

// DefaultPluginsDir is where plugins are installed unless they declare a destination
const DefaultPluginsDir = "plugins"

// DefaultServerName is the file name of the server jar unless the template names it
const DefaultServerName = "server.jar"

// Template is one templates/<name>.yaml file. A template carrying an id can be built; one without
// is a partial only meant to be used as a parent.
type Template struct {
	Schema         string    `yaml:"$schema,omitempty" json:"-"`
	Version        int       `yaml:"version" json:"version"`
	ID             string    `yaml:"id,omitempty" json:"id,omitempty"`
	Parents        []string  `yaml:"parents,omitempty" json:"parents,omitempty"`
	Server         *Artifact `yaml:"server,omitempty" json:"server,omitempty"`
	Plugins        []Plugin  `yaml:"plugins,omitempty" json:"plugins,omitempty"`
	Worlds         []World   `yaml:"worlds,omitempty" json:"worlds,omitempty"`
	Files          []string  `yaml:"files,omitempty" json:"files,omitempty"`
	Patches        []Patch   `yaml:"patches,omitempty" json:"patches,omitempty"`
	RuntimePatches []Patch   `yaml:"runtimePatches,omitempty" json:"runtimePatches,omitempty"`

	// Path of the file the template was loaded from
	Path string `yaml:"-" json:"-"`
}

// Artifact locates a file to install. Exactly one of Download, URL or Source+Path is set.
type Artifact struct {
	// File name the artifact is installed as (plugins and server) or directory name (worlds)
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	// Directory relative to the server root the artifact is installed in
	Destination string `yaml:"destination,omitempty" json:"destination,omitempty"`

	Download *Download `yaml:"download,omitempty" json:"download,omitempty"`
	URL      string    `yaml:"url,omitempty" json:"url,omitempty"`
	Source   string    `yaml:"source,omitempty" json:"source,omitempty"`
	Path     string    `yaml:"path,omitempty" json:"path,omitempty"`

	// Expected SHA-256 of the downloaded file, checked on top of what the source publishes
	SHA256 string `yaml:"sha256,omitempty" json:"sha256,omitempty"`
	// The artifact is only installed when this environment variable is "true" or "1"
	RequiredEnv string `yaml:"requiredEnv,omitempty" json:"requiredEnv,omitempty"`
}

// Plugin is a jar installed in the plugins directory, keyed by JarID so that a child template can
// replace or remove the plugin a parent declares
type Plugin struct {
	JarID    string `yaml:"jarId" json:"jarId"`
	Remove   bool   `yaml:"remove,omitempty" json:"remove,omitempty"`
	Artifact `yaml:",inline" json:",inline"`
}

// World is a directory of the server root, installed from an archive or a directory, keyed by Name
type World struct {
	Remove bool `yaml:"remove,omitempty" json:"remove,omitempty"`
	// Extract the downloaded file: zip, or nothing for a source serving a directory
	Extract string `yaml:"extract,omitempty" json:"extract,omitempty"`
	// Number of leading path components dropped from the archive entries
	StripComponents int `yaml:"stripComponents,omitempty" json:"stripComponents,omitempty"`
	// Replace the world when it already exists. Worlds are kept by default: a running server owns them.
	Overwrite bool `yaml:"overwrite,omitempty" json:"overwrite,omitempty"`
	Artifact  `yaml:",inline" json:",inline"`
}

// Download resolves an artifact through a typed resolver. Which fields apply depends on Type.
type Download struct {
	Type    string `yaml:"type" json:"type"`
	Version string `yaml:"version,omitempty" json:"version,omitempty"`

	// maven, gitlab-generic: named source to read from
	Source string `yaml:"source,omitempty" json:"source,omitempty"`
	// maven: repository URL (instead of a source) or Nexus repository name; github: owner/repo
	Repository string `yaml:"repository,omitempty" json:"repository,omitempty"`
	// maven (with repository), github: named credential
	Credentials string `yaml:"credentials,omitempty" json:"credentials,omitempty"`

	// maven
	GroupID    string `yaml:"groupId,omitempty" json:"groupId,omitempty"`
	ArtifactID string `yaml:"artifactId,omitempty" json:"artifactId,omitempty"`
	Classifier string `yaml:"classifier,omitempty" json:"classifier,omitempty"`
	Extension  string `yaml:"extension,omitempty" json:"extension,omitempty"`

	// papermc, modrinth: project; gitlab (maven, gitlab-generic): project overriding the source one
	Project string `yaml:"project,omitempty" json:"project,omitempty"`
	// gitlab maven: group overriding the source one
	Group string `yaml:"group,omitempty" json:"group,omitempty"`
	// papermc
	Build   int    `yaml:"build,omitempty" json:"build,omitempty"`
	Channel string `yaml:"channel,omitempty" json:"channel,omitempty"`

	// modrinth
	Loaders      []string `yaml:"loaders,omitempty" json:"loaders,omitempty"`
	GameVersions []string `yaml:"gameVersions,omitempty" json:"gameVersions,omitempty"`

	// github, modrinth: regular expression the file name must fully match
	Asset string `yaml:"asset,omitempty" json:"asset,omitempty"`

	// luckperms
	Platform string `yaml:"platform,omitempty" json:"platform,omitempty"`

	// gitlab-generic
	Package string `yaml:"package,omitempty" json:"package,omitempty"`
	File    string `yaml:"file,omitempty" json:"file,omitempty"`
}

// Patch sets fields of a configuration file (.properties, .yml, .yaml or .json)
type Patch struct {
	File   string  `yaml:"file" json:"file"`
	Fields []Field `yaml:"fields" json:"fields"`
}

// Field is a dotted path (proxies.velocity.secret) and the scalar written there. String values
// may reference environment variables: ${NAME} or ${NAME:-default}.
type Field struct {
	Field string `yaml:"field" json:"field"`
	Value any    `yaml:"value" json:"value"`
	// Type the value is written as once interpolated: string, int, float or bool. Defaults to the
	// type the value is written with in the template.
	Type string `yaml:"type,omitempty" json:"type,omitempty"`
}

// Buildable reports whether the template can be built on its own
func (t *Template) Buildable() bool {
	return t.ID != ""
}

// ServerName is the file name of the server jar
func (t *Template) ServerName() string {
	if t.Server != nil && t.Server.Name != "" {
		return t.Server.Name
	}
	return DefaultServerName
}

// VersionOf returns the version an artifact asks for, empty when it is pinned by URL or path
func (a *Artifact) VersionOf() string {
	if a.Download == nil {
		return ""
	}
	return a.Download.Version
}
