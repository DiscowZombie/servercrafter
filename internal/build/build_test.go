package build

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discowzombie/servercrafter/internal/bootstrap"
	"github.com/discowzombie/servercrafter/internal/fetch"
	"github.com/discowzombie/servercrafter/internal/manifest"
	"github.com/discowzombie/servercrafter/internal/project"
	"github.com/discowzombie/servercrafter/internal/resolve"
	"github.com/discowzombie/servercrafter/internal/template"
	"github.com/sirupsen/logrus"
)

type counter struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *counter) add(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[path]++
}

func (c *counter) get(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[path]
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range files {
		f, _ := w.Create(name)
		_, _ = f.Write([]byte(content))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func write(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type fixture struct {
	project *project.Project
	loader  *template.Loader
	out     string
	calls   *counter
	log     *logrus.Logger
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	calls := &counter{calls: map[string]int{}}
	world := zipBytes(t, map[string]string{"lobby/level.dat": "level", "lobby/region/r.0.0.mca": "region"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.add(r.URL.Path)
		switch r.URL.Path {
		case "/paper.jar":
			_, _ = io.WriteString(w, "paper")
		case "/repo/a.jar", "/repo/b.jar", "/repo/c.jar":
			_, _ = io.WriteString(w, "plugin "+filepath.Base(r.URL.Path))
		case "/repo/lobby.zip":
			_, _ = w.Write(world)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	dir := t.TempDir()
	write(t, filepath.Join(dir, "servercrafter.yaml"), `
version: 1
sources:
  repo: { type: http, baseUrl: `+server.URL+`/repo/ }
`)
	write(t, filepath.Join(dir, "templates", "_base.yaml"), `
version: 1
server: { url: `+server.URL+`/paper.jar, sha256: 7a9b2d4bbd2ad0b4e1d1d4e8e4ce6a4cb2c0d4ad3c6d3c0e0b5a4d5b3e9b8c7c }
plugins:
  - { jarId: a, name: A.jar, source: repo, path: a.jar }
  - { jarId: b, name: B.jar, source: repo, path: b.jar }
files: [files/base]
patches:
  - file: server.properties
    fields: [{ field: max-players, value: 50 }]
runtimePatches:
  - file: config/redis.yml
    fields: [{ field: singleServerConfig.password, value: "${CFG_REDIS_PASSWORD}" }]
`)
	write(t, filepath.Join(dir, "templates", "lobby.yaml"), `
version: 1
id: lobby
parents: [base]
plugins:
  - { jarId: c, name: C.jar, destination: plugins/Core/modules, source: repo, path: c.jar }
worlds:
  - { name: world, source: repo, path: lobby.zip, extract: zip, stripComponents: 1 }
files: [files/lobby]
patches:
  - file: server.properties
    fields: [{ field: motd, value: "${MOTD:-Lobby}" }]
`)
	write(t, filepath.Join(dir, "files", "base", "server.properties"), "# props\nmotd=A Minecraft Server\nmax-players=20\n")
	write(t, filepath.Join(dir, "files", "base", "config", "redis.yml"), "singleServerConfig:\n  password: replace-at-runtime\n  address: redis://redis:6379\n")
	write(t, filepath.Join(dir, "files", "lobby", "plugins", "A", "config.yml"), "enabled: true\n")

	p, err := project.Load(filepath.Join(dir, "servercrafter.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	log := logrus.New()
	log.SetOutput(io.Discard)
	return &fixture{project: p, loader: template.NewLoader(p.TemplatesPath()), out: filepath.Join(dir, "server"), calls: calls, log: log}
}

func (f *fixture) options(t *testing.T, id string) Options {
	t.Helper()
	f.loader = template.NewLoader(f.project.TemplatesPath())
	leaf, err := f.loader.Find(id)
	if err != nil {
		t.Fatal(err)
	}
	flat, err := f.loader.Flatten(leaf)
	if err != nil {
		t.Fatal(err)
	}
	client := fetch.NewClient("test", f.log)
	client.Backoff = time.Millisecond
	return Options{
		Project:  f.project,
		Template: flat,
		Resolver: resolve.New(f.project, client, f.log),
		OutDir:   f.out,
		Env:      func(string) (string, bool) { return "", false },
		Log:      f.log,
		Tool:     "test",
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestBuild(t *testing.T) {
	f := newFixture(t)
	// The server jar declares a wrong checksum: the build must fail and install nothing
	opts := f.options(t, "lobby")
	if _, err := Run(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("expected a checksum failure, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.out, "server.jar")); err == nil {
		t.Fatal("a jar failing its checksum was installed")
	}

	paperSHA, err := fetchSum("paper")
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(f.project.TemplatesPath(), "_base.yaml")
	content := strings.Replace(read(t, base), "7a9b2d4bbd2ad0b4e1d1d4e8e4ce6a4cb2c0d4ad3c6d3c0e0b5a4d5b3e9b8c7c", paperSHA, 1)
	write(t, base, content)

	m, err := Run(context.Background(), f.options(t, "lobby"))
	if err != nil {
		t.Fatal(err)
	}
	expect := map[string]string{
		"server.jar":                 "paper",
		"plugins/A.jar":              "plugin a.jar",
		"plugins/B.jar":              "plugin b.jar",
		"plugins/Core/modules/C.jar": "plugin c.jar",
		"plugins/A/config.yml":       "enabled: true\n",
		"world/level.dat":            "level",
		"world/region/r.0.0.mca":     "region",
		"server.properties":          "# props\nmotd=Lobby\nmax-players=50\n",
		"config/redis.yml":           "singleServerConfig:\n  password: replace-at-runtime\n  address: redis://redis:6379\n",
	}
	for path, want := range expect {
		if got := read(t, filepath.Join(f.out, path)); got != want {
			t.Errorf("%s: got %q, want %q", path, got, want)
		}
	}
	if len(m.Entries) != 5 || len(m.RuntimePatches) != 1 {
		t.Errorf("manifest: %+v", m)
	}
	server, _ := m.Find("server")
	if server.SHA256 != paperSHA {
		t.Errorf("server entry: %+v", server)
	}

	// A second build downloads only what it cannot prove up to date: the server jar has a
	// checksum, the plugins have no version and are fetched again, the world is kept
	_ = os.WriteFile(filepath.Join(f.out, "world", "playerdata.dat"), []byte("players"), 0o644)
	paperCalls, worldCalls, pluginCalls := f.calls.get("/paper.jar"), f.calls.get("/repo/lobby.zip"), f.calls.get("/repo/a.jar")
	if _, err = Run(context.Background(), f.options(t, "lobby")); err != nil {
		t.Fatal(err)
	}
	if f.calls.get("/paper.jar") != paperCalls {
		t.Error("the server jar was downloaded again despite matching its checksum")
	}
	if f.calls.get("/repo/lobby.zip") != worldCalls {
		t.Error("the existing world was downloaded again")
	}
	if f.calls.get("/repo/a.jar") != pluginCalls+1 {
		t.Error("a plugin with no version nor checksum must be downloaded again")
	}
	if read(t, filepath.Join(f.out, "world", "playerdata.dat")) != "players" {
		t.Error("the existing world was replaced")
	}

	// Bootstrap verifies the files and applies the runtime patches
	env := map[string]string{"CFG_REDIS_PASSWORD": "s3cr3t"}
	bopts := bootstrap.Options{Target: f.out, Verify: true, Log: f.log,
		Env: func(name string) (string, bool) { v, ok := env[name]; return v, ok }}
	if err = bootstrap.Prepare(bopts); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(f.out, "config", "redis.yml")); !strings.Contains(got, "password: s3cr3t") {
		t.Errorf("runtime patch not applied: %s", got)
	}
	delete(env, "CFG_REDIS_PASSWORD")
	if err = bootstrap.Prepare(bopts); err == nil || !strings.Contains(err.Error(), "CFG_REDIS_PASSWORD") {
		t.Errorf("an unset variable must fail the bootstrap: %v", err)
	}
	_ = os.WriteFile(filepath.Join(f.out, "plugins", "B.jar"), []byte("tampered"), 0o644)
	if err = bootstrap.Prepare(bopts); err == nil || !strings.Contains(err.Error(), "plugins/B.jar") {
		t.Errorf("a modified jar must fail the verification: %v", err)
	}

	// Diff reports the modified jar
	changes, err := Diff(context.Background(), f.options(t, "lobby"))
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, c := range changes {
		statuses[c.ID] = c.Status
	}
	if statuses["plugin:b"] != StatusModified || statuses["server"] != StatusUnchanged || statuses["world:world"] != StatusUnchanged {
		t.Errorf("diff: %v", statuses)
	}

	// Dropping a plugin from the template removes its jar on the next build
	leaf := filepath.Join(f.project.TemplatesPath(), "lobby.yaml")
	write(t, leaf, strings.Replace(read(t, leaf), "plugins:\n  - { jarId: c,", "plugins:\n  - { jarId: b, remove: true }\n  - { jarId: c,", 1))
	if _, err = Run(context.Background(), f.options(t, "lobby")); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(f.out, "plugins", "B.jar")); err == nil {
		t.Error("the removed plugin is still installed")
	}
	if m, _ = manifest.ReadIfExists(f.out); len(m.Entries) != 4 {
		t.Errorf("manifest after removal: %+v", m.Entries)
	}
}

func fetchSum(content string) (string, error) {
	dir, err := os.MkdirTemp("", "sum")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "f")
	if err = os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return fetch.FileSHA256(path)
}

func TestOverrides(t *testing.T) {
	f := newFixture(t)
	opts := f.options(t, "lobby")
	if _, err := Plan(opts.Template, map[string]string{"nope": "1"}, opts.Env, f.log); err == nil ||
		!strings.Contains(err.Error(), "match no artifact") {
		t.Errorf("unknown override: %v", err)
	}
	if _, err := Plan(opts.Template, map[string]string{"a": "1"}, opts.Env, f.log); err == nil ||
		!strings.Contains(err.Error(), "pinned by url or path") {
		t.Errorf("pinned override: %v", err)
	}
	tpl := &template.Template{Version: 1, ID: "x", Server: &template.Artifact{Download: &template.Download{Type: "papermc", Project: "paper", Version: "1.21.4", Build: 3}}}
	items, err := Plan(tpl, map[string]string{"server": "1.21.8"}, opts.Env, f.log)
	if err != nil {
		t.Fatal(err)
	}
	if d := items[0].Artifact.Download; d.Version != "1.21.8" || d.Build != 0 {
		t.Errorf("override: %+v", d)
	}
	if tpl.Server.Download.Version != "1.21.4" {
		t.Error("the override modified the template")
	}
}

func TestRequiredEnv(t *testing.T) {
	tpl := &template.Template{Version: 1, ID: "x",
		Server:  &template.Artifact{URL: "https://example.com/s.jar"},
		Plugins: []template.Plugin{{JarID: "dev", Artifact: template.Artifact{Name: "dev.jar", URL: "https://example.com/d.jar", RequiredEnv: "WITH_DEV"}}}}
	log := logrus.New()
	log.SetOutput(io.Discard)
	for value, want := range map[string]int{"": 1, "false": 1, "true": 2, "1": 2} {
		env := func(string) (string, bool) { return value, value != "" }
		items, err := Plan(tpl, nil, env, log)
		if err != nil || len(items) != want {
			t.Errorf("WITH_DEV=%q: %d items, %v", value, len(items), err)
		}
	}
}

func TestVersionInterpolation(t *testing.T) {
	tpl := &template.Template{Version: 1, ID: "x",
		Server: &template.Artifact{Download: &template.Download{Type: "maven", Source: "nexus",
			GroupID: "g", ArtifactID: "a", Version: "${BRANCH:-prod}"}},
		Plugins: []template.Plugin{{JarID: "p", Artifact: template.Artifact{Name: "p.jar",
			Download: &template.Download{Type: "maven", Source: "nexus", GroupID: "g", ArtifactID: "p", Version: "${REQUIRED}"}}}}}
	log := logrus.New()
	log.SetOutput(io.Discard)
	env := func(vars map[string]string) func(string) (string, bool) {
		return func(name string) (string, bool) { v, ok := vars[name]; return v, ok }
	}

	items, err := Plan(tpl, nil, env(map[string]string{"REQUIRED": "1.0"}), log)
	if err != nil {
		t.Fatal(err)
	}
	if v := items[0].Artifact.Download.Version; v != "prod" {
		t.Errorf("default: %q", v)
	}
	if v := items[1].Artifact.Download.Version; v != "1.0" {
		t.Errorf("set: %q", v)
	}
	if tpl.Server.Download.Version != "${BRANCH:-prod}" {
		t.Error("the interpolation modified the template")
	}

	items, err = Plan(tpl, nil, env(map[string]string{"BRANCH": "dev", "REQUIRED": "1.0"}), log)
	if err != nil || items[0].Artifact.Download.Version != "dev" {
		t.Errorf("BRANCH=dev: %v", err)
	}

	if _, err := Plan(tpl, nil, env(nil), log); err == nil || !strings.Contains(err.Error(), "REQUIRED") {
		t.Errorf("unset variable: %v", err)
	}
	// An override replaces the version before it is interpolated
	if _, err := Plan(tpl, map[string]string{"p": "2.0"}, env(nil), log); err != nil {
		t.Errorf("override of an unset variable: %v", err)
	}
}

func TestCleanRefusesForeignDirectories(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "important.txt"), "data")
	if err := clean(dir); err == nil {
		t.Fatal("clean emptied a directory servercrafter did not build")
	}
	write(t, filepath.Join(dir, manifest.FileName), "{}")
	if err := clean(dir); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left %v", entries)
	}
}
