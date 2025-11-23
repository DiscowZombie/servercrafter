package resolve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discowzombie/servercrafter/internal/fetch"
	"github.com/discowzombie/servercrafter/internal/project"
	"github.com/discowzombie/servercrafter/internal/template"
	"github.com/sirupsen/logrus"
)

// fakeAPI serves canned responses by path (and query when the key carries one), recording requests
type fakeAPI struct {
	t        *testing.T
	routes   map[string]string
	requests []*http.Request
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests = append(f.requests, r)
	for _, key := range []string{r.URL.Path + "?" + r.URL.RawQuery, r.URL.EscapedPath(), r.URL.Path} {
		if body, ok := f.routes[key]; ok {
			_, _ = io.WriteString(w, body)
			return
		}
	}
	http.NotFound(w, r)
}

func newResolver(t *testing.T, p *project.Project, routes map[string]string) (*Resolver, *fakeAPI, string) {
	t.Helper()
	api := &fakeAPI{t: t, routes: routes}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	log := logrus.New()
	log.SetOutput(io.Discard)
	client := fetch.NewClient("test", log)
	client.Backoff = time.Millisecond
	if p == nil {
		p = project.Implicit(t.TempDir())
	}
	r := New(p, client, log)
	r.APIs = APIs{
		PaperMC:   server.URL + "/papermc",
		Modrinth:  server.URL + "/modrinth",
		Github:    server.URL + "/github",
		LuckPerms: server.URL + "/luckperms",
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, api, server.URL
}

func resolveDownload(t *testing.T, r *Resolver, d template.Download) *Resolved {
	t.Helper()
	res, err := r.Resolve(context.Background(), &template.Artifact{Download: &d})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func read(t *testing.T, res *Resolved) string {
	t.Helper()
	rc, err := res.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	return string(data)
}

const artifactMetadata = `<metadata><versioning><latest>1.3.0-SNAPSHOT</latest><release>1.2.0</release>
<versions><version>1.1.0</version><version>1.2.0</version><version>1.3.0-SNAPSHOT</version></versions></versioning></metadata>`

const snapshotMetadata = `<metadata><versioning><snapshotVersions>
<snapshotVersion><classifier>all</classifier><extension>jar</extension><value>1.3.0-20260730.120000-2</value></snapshotVersion>
<snapshotVersion><extension>jar</extension><value>1.3.0-20260730.120000-2</value></snapshotVersion>
<snapshotVersion><extension>pom</extension><value>1.3.0-20260730.120000-2</value></snapshotVersion>
</snapshotVersions></versioning></metadata>`

func TestMavenRepository(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	r, api, base := newResolver(t, nil, map[string]string{
		"/m2/fr/discow/core/maven-metadata.xml":                                         artifactMetadata,
		"/m2/fr/discow/core/1.3.0-SNAPSHOT/maven-metadata.xml":                          snapshotMetadata,
		"/m2/fr/discow/core/1.3.0-SNAPSHOT/core-1.3.0-20260730.120000-2-all.jar":        "snapshot-all",
		"/m2/fr/discow/core/1.3.0-SNAPSHOT/core-1.3.0-20260730.120000-2-all.jar.sha256": sha + "  core.jar\n",
		"/m2/fr/discow/core/1.2.0/core-1.2.0.jar":                                       "release",
	})
	d := template.Download{Type: "maven", Repository: base + "/m2", GroupID: "fr.discow", ArtifactID: "core", Version: "latest-snapshot", Classifier: "all"}
	res := resolveDownload(t, r, d)
	if res.Version != "1.3.0-20260730.120000-2" || read(t, res) != "snapshot-all" || res.Checksums["sha256"] != sha {
		t.Errorf("snapshot: %+v", res)
	}
	for _, version := range []string{"release", "latest-release", "1.2.0"} {
		d = template.Download{Type: "maven", Repository: base + "/m2", GroupID: "fr.discow", ArtifactID: "core", Version: version}
		res = resolveDownload(t, r, d)
		if res.Version != "1.2.0" || read(t, res) != "release" {
			t.Errorf("%s: %+v", version, res)
		}
	}
	if !strings.HasSuffix(res.Origin, "/m2/fr/discow/core/1.2.0/core-1.2.0.jar") {
		t.Errorf("origin: %s", res.Origin)
	}
	_ = api
}

func TestGitlabScopes(t *testing.T) {
	p := project.Implicit(t.TempDir())
	r, api, base := newResolver(t, p, map[string]string{
		"/api/v4/groups/mc/-/packages/maven/fr/discow/core/1.0.0/core-1.0.0.jar":        "group",
		"/api/v4/projects/mc%2Fcore/packages/maven/fr/discow/core/1.0.0/core-1.0.0.jar": "project",
		"/api/v4/projects/42/packages/generic/maps/1.0.0/lobby.zip":                     "generic",
	})
	t.Setenv("TEST_GITLAB_TOKEN", "glpat")
	p.Credentials = map[string]*project.Credential{
		"gl": {Type: project.CredentialHeader, Name: "PRIVATE-TOKEN", Value: &project.Value{Env: "TEST_GITLAB_TOKEN"}},
	}
	p.Sources = map[string]*project.Source{"gl": {Type: project.SourceGitlab, BaseURL: base, Group: "mc", Credentials: "gl"}}
	d := template.Download{Type: "maven", Source: "gl", GroupID: "fr.discow", ArtifactID: "core", Version: "1.0.0"}
	if got := read(t, resolveDownload(t, r, d)); got != "group" {
		t.Errorf("group scope: %s", got)
	}
	d.Project = "mc/core"
	if got := read(t, resolveDownload(t, r, d)); got != "project" {
		t.Errorf("project scope: %s", got)
	}
	g := template.Download{Type: "gitlab-generic", Source: "gl", Project: "42", Package: "maps", Version: "1.0.0", File: "lobby.zip"}
	if got := read(t, resolveDownload(t, r, g)); got != "generic" {
		t.Errorf("generic: %s", got)
	}
	last := api.requests[len(api.requests)-1]
	if last.Header.Get("PRIVATE-TOKEN") != "glpat" {
		t.Errorf("token not sent: %v", last.Header)
	}
}

func TestNexusSearch(t *testing.T) {
	p := project.Implicit(t.TempDir())
	search := `{"items":[
		{"downloadUrl":"%[1]s/repository/r/core-2.0-sources.jar","checksum":{"sha1":"%[2]s"},"maven2":{"version":"2.0","classifier":"sources","extension":"jar"}},
		{"downloadUrl":"%[1]s/repository/r/core-2.0.jar","checksum":{"sha1":"%[2]s"},"maven2":{"version":"2.0","extension":"jar"}}]}`
	r, api, base := newResolver(t, p, nil)
	api.routes = map[string]string{
		"/service/rest/v1/search/assets": fmt.Sprintf(search, base, strings.Repeat("a", 40)),
		"/repository/r/core-2.0.jar":     "core",
		"/repository/raw/maps/a b.zip":   "raw",
	}
	p.Sources = map[string]*project.Source{"nexus": {Type: project.SourceNexus, BaseURL: base, Repository: "raw"}}
	res := resolveDownload(t, r, template.Download{Type: "maven", Source: "nexus", Repository: "r", GroupID: "fr.discow", ArtifactID: "core", Version: "latest-release"})
	if res.Version != "2.0" || read(t, res) != "core" || res.Checksums["sha1"] != strings.Repeat("a", 40) {
		t.Errorf("search: %+v", res)
	}
	query := api.requests[0].URL.Query()
	if query.Get("prerelease") != "false" || query.Get("repository") != "r" || query.Has("maven.classifier") || query.Has("maven.baseVersion") {
		t.Errorf("query: %v", query)
	}
	path, err := r.Resolve(context.Background(), &template.Artifact{Source: "nexus", Path: "maps/a b.zip"})
	if err != nil || read(t, path) != "raw" {
		t.Errorf("raw path: %v", err)
	}
}

func TestPaperMC(t *testing.T) {
	build := func(id int, channel string) map[string]any {
		return map[string]any{"id": id, "channel": channel, "downloads": map[string]any{"server:default": map[string]any{
			"name": fmt.Sprintf("paper-%d.jar", id), "url": fmt.Sprintf("https://fill-data.papermc.io/paper-%d.jar", id),
			"checksums": map[string]string{"sha256": strings.Repeat("c", 64)}}}}
	}
	marshal := func(v any) string { data, _ := json.Marshal(v); return string(data) }
	r, _, _ := newResolver(t, nil, map[string]string{
		"/papermc/projects/paper/versions": marshal(map[string]any{"versions": []any{
			map[string]any{"version": map[string]string{"id": "26.3-rc-3"}, "builds": []int{1}},
			map[string]any{"version": map[string]string{"id": "26.2"}, "builds": []int{1, 2}},
			map[string]any{"version": map[string]string{"id": "26.1"}, "builds": []int{9}},
		}}),
		"/papermc/projects/paper/versions/26.2/builds":       marshal([]any{build(2, "ALPHA"), build(1, "STABLE")}),
		"/papermc/projects/paper/versions/1.21.4/builds/232": marshal(build(232, "STABLE")),
	})
	res := resolveDownload(t, r, template.Download{Type: "papermc", Project: "paper"})
	if res.Version != "26.2-1" || res.Origin != "https://fill-data.papermc.io/paper-1.jar" || res.Checksums["sha256"] == "" {
		t.Errorf("latest stable: %+v", res)
	}
	res = resolveDownload(t, r, template.Download{Type: "papermc", Project: "paper", Version: "26.2", Channel: "alpha"})
	if res.Version != "26.2-2" {
		t.Errorf("alpha channel: %+v", res)
	}
	res = resolveDownload(t, r, template.Download{Type: "papermc", Project: "paper", Version: "1.21.4", Build: 232})
	if res.Version != "1.21.4-232" {
		t.Errorf("pinned build: %+v", res)
	}
}

func TestModrinth(t *testing.T) {
	versions := `[
		{"id":"b","version_number":"2.0-beta","version_type":"beta","files":[{"url":"https://cdn.modrinth.com/b.jar","filename":"b.jar","primary":true,"hashes":{"sha1":"%[1]s"}}]},
		{"id":"a","version_number":"1.0","version_type":"release","files":[
			{"url":"https://cdn.modrinth.com/a-sources.jar","filename":"a-sources.jar","primary":false,"hashes":{}},
			{"url":"https://cdn.modrinth.com/a.jar","filename":"a.jar","primary":true,"hashes":{"sha512":"%[2]s","sha1":"%[1]s"}}]}]`
	r, api, _ := newResolver(t, nil, map[string]string{
		"/modrinth/project/fawe/version": fmt.Sprintf(versions, strings.Repeat("1", 40), strings.Repeat("5", 128)),
	})
	res := resolveDownload(t, r, template.Download{Type: "modrinth", Project: "fawe", Loaders: []string{"paper"}, GameVersions: []string{"1.21.4"}})
	if res.Version != "1.0" || res.Origin != "https://cdn.modrinth.com/a.jar" || len(res.Checksums) != 2 {
		t.Errorf("latest release: %+v", res)
	}
	if q := api.requests[0].URL.Query(); q.Get("loaders") != `["paper"]` || q.Get("game_versions") != `["1.21.4"]` {
		t.Errorf("query: %v", q)
	}
	res = resolveDownload(t, r, template.Download{Type: "modrinth", Project: "fawe", Version: "latest-published"})
	if res.Version != "2.0-beta" {
		t.Errorf("latest published: %+v", res)
	}
	res = resolveDownload(t, r, template.Download{Type: "modrinth", Project: "fawe", Version: "1.0", Asset: `a-sources\.jar`})
	if res.Origin != "https://cdn.modrinth.com/a-sources.jar" {
		t.Errorf("asset: %+v", res)
	}
}

func TestGithub(t *testing.T) {
	p := project.Implicit(t.TempDir())
	r, api, base := newResolver(t, p, nil)
	release := func(tag string, prerelease bool) map[string]any {
		return map[string]any{"tag_name": tag, "prerelease": prerelease, "assets": []any{
			map[string]any{"name": "echo-" + tag + "-sources.jar", "url": base + "/asset/1", "browser_download_url": "https://github.com/dl/1"},
			map[string]any{"name": "echo-" + tag + "-all.jar", "url": base + "/asset/2", "browser_download_url": "https://github.com/dl/2",
				"digest": "sha256:" + strings.Repeat("d", 64)},
		}}
	}
	marshal := func(v any) string { data, _ := json.Marshal(v); return string(data) }
	api.routes = map[string]string{
		"/github/repos/o/echo/releases/latest": marshal(release("1.0", false)),
		"/github/repos/o/echo/releases":        marshal([]any{map[string]any{"tag_name": "draft", "draft": true}, release("1.1-rc", true)}),
		"/asset/2":                             "private",
	}
	d := template.Download{Type: "github", Repository: "o/echo", Asset: `echo-.*-all\.jar`}
	res := resolveDownload(t, r, d)
	if res.Version != "1.0" || res.Origin != "https://github.com/dl/2" || res.Checksums["sha256"] != strings.Repeat("d", 64) {
		t.Errorf("latest: %+v", res)
	}
	d.Version = "latest-published"
	if res = resolveDownload(t, r, d); res.Version != "1.1-rc" {
		t.Errorf("latest published: %+v", res)
	}
	t.Setenv("TEST_GH_TOKEN", "ghp")
	p.Credentials = map[string]*project.Credential{"gh": {Type: project.CredentialBearer, Token: &project.Value{Env: "TEST_GH_TOKEN"}}}
	d.Credentials = "gh"
	res = resolveDownload(t, r, d)
	if read(t, res) != "private" {
		t.Error("private assets are read through the API")
	}
	last := api.requests[len(api.requests)-1]
	if last.Header.Get("Accept") != "application/octet-stream" || last.Header.Get("Authorization") != "Bearer ghp" {
		t.Errorf("asset request headers: %v", last.Header)
	}
	d.Asset = `nothing\.jar`
	if _, err := r.Resolve(context.Background(), &template.Artifact{Download: &d}); err == nil || !strings.Contains(err.Error(), "echo-1.1-rc-all.jar") {
		t.Errorf("missing asset error should list the assets: %v", err)
	}
}

func TestLuckPerms(t *testing.T) {
	r, _, _ := newResolver(t, nil, map[string]string{
		"/luckperms": `{"version":"5.5.91","downloads":{"bukkit":"https://download.luckperms.net/1678/bukkit/LuckPerms-Bukkit-5.5.91.jar","velocity":"https://v"}}`,
	})
	res := resolveDownload(t, r, template.Download{Type: "luckperms", Platform: "bukkit"})
	if res.Version != "5.5.91" || !strings.HasSuffix(res.Origin, "LuckPerms-Bukkit-5.5.91.jar") {
		t.Errorf("%+v", res)
	}
	if _, err := r.Resolve(context.Background(), &template.Artifact{Download: &template.Download{Type: "luckperms", Platform: "nope"}}); err == nil ||
		!strings.Contains(err.Error(), "bukkit, velocity") {
		t.Errorf("unknown platform: %v", err)
	}
}

func TestS3Signing(t *testing.T) {
	p := project.Implicit(t.TempDir())
	r, api, base := newResolver(t, p, map[string]string{"/bucket/maps/lobby.zip": "zip"})
	t.Setenv("TEST_AK", "AKID")
	t.Setenv("TEST_SK", "secret")
	p.Credentials = map[string]*project.Credential{"s3": {Type: project.CredentialS3, AccessKey: &project.Value{Env: "TEST_AK"},
		SecretKey: &project.Value{Env: "TEST_SK"}, Region: "fr-par"}}
	p.Sources = map[string]*project.Source{"bucket": {Type: project.SourceS3, BaseURL: base + "/bucket/", Credentials: "s3"}}
	res, err := r.Resolve(context.Background(), &template.Artifact{Source: "bucket", Path: "maps/lobby.zip"})
	if err != nil || read(t, res) != "zip" {
		t.Fatal(err)
	}
	authorization := api.requests[0].Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "AWS4-HMAC-SHA256 Credential=AKID/") || !strings.Contains(authorization, "/fr-par/s3/aws4_request") {
		t.Errorf("authorization: %s", authorization)
	}
}

func TestMissingSecretFailsOnUse(t *testing.T) {
	p := project.Implicit(t.TempDir())
	p.Credentials = map[string]*project.Credential{"c": {Type: project.CredentialBearer, Token: &project.Value{Env: "TEST_UNSET_TOKEN"}}}
	p.Sources = map[string]*project.Source{"s": {Type: project.SourceHTTP, BaseURL: "https://example.com", Credentials: "c"}}
	r, _, _ := newResolver(t, p, nil)
	_, err := r.Resolve(context.Background(), &template.Artifact{Source: "s", Path: "a.jar"})
	if err == nil || !strings.Contains(err.Error(), "TEST_UNSET_TOKEN is not set") {
		t.Errorf("got %v", err)
	}
}

func TestLocalSource(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "static", "worlds", "lobby"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "static", "a.jar"), []byte("a"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "secret"), []byte("s"), 0o644)
	p := project.Implicit(dir)
	p.Sources = map[string]*project.Source{"static": {Type: project.SourceLocal, Directory: "static"}}
	r, _, _ := newResolver(t, p, nil)
	res, err := r.Resolve(context.Background(), &template.Artifact{Source: "static", Path: "a.jar", SHA256: "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb"})
	if err != nil || read(t, res) != "a" {
		t.Fatal(err)
	}
	if res, err = r.Resolve(context.Background(), &template.Artifact{Source: "static", Path: "worlds/lobby"}); err != nil || res.Dir == "" {
		t.Errorf("directory: %+v, %v", res, err)
	}
	// The path is confined to the source directory
	if _, err = r.Resolve(context.Background(), &template.Artifact{Source: "static", Path: "../secret"}); err == nil {
		t.Error("a path escaped the source directory")
	}
}

// Nexus matches maven.baseVersion token by token: "test" also returns "feat-rollout-cmd-test"
func TestNexusExactVersion(t *testing.T) {
	p := project.Implicit(t.TempDir())
	search := `{"items":[
		{"downloadUrl":"%[1]s/repository/r/core-feat-rollout-cmd-test.jar","checksum":{"sha1":"%[2]s"},"maven2":{"version":"feat-rollout-cmd-test","extension":"jar"}},
		{"downloadUrl":"%[1]s/repository/r/core-test-20260730.120000-2.jar","checksum":{"sha1":"%[2]s"},"maven2":{"version":"test-20260730.120000-2","extension":"jar"}},
		{"downloadUrl":"%[1]s/repository/r/core-test.jar","checksum":{"sha1":"%[2]s"},"maven2":{"version":"test","extension":"jar"}}]}`
	r, api, base := newResolver(t, p, nil)
	api.routes = map[string]string{
		"/service/rest/v1/search/assets": fmt.Sprintf(search, base, strings.Repeat("a", 40)),
		"/repository/r/core-test.jar":    "core",
	}
	p.Sources = map[string]*project.Source{"nexus": {Type: project.SourceNexus, BaseURL: base}}
	res := resolveDownload(t, r, template.Download{Type: "maven", Source: "nexus", Repository: "r", GroupID: "fr.discow", ArtifactID: "core", Version: "test"})
	if res.Version != "test" || read(t, res) != "core" {
		t.Errorf("exact: %+v", res)
	}
	if _, err := r.Resolve(context.Background(), &template.Artifact{Download: &template.Download{Type: "maven", Source: "nexus", Repository: "r", GroupID: "fr.discow", ArtifactID: "core", Version: "cmd"}}); err == nil {
		t.Error("cmd matched a partial version")
	}
}

func TestMavenBaseVersion(t *testing.T) {
	for in, want := range map[string]string{"1.0": "1.0", "1.0-SNAPSHOT": "1.0-SNAPSHOT", "1.0-20260730.120000-2": "1.0-SNAPSHOT", "feat-x": "feat-x"} {
		if got := mavenBaseVersion(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}
