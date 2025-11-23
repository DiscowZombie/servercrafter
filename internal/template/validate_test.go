package template

import (
	"strings"
	"testing"

	"github.com/discowzombie/servercrafter/internal/yamlutil"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string // substring of the error, empty when valid
	}{
		{
			name: "valid",
			yaml: `
version: 1
id: lobby
server: { download: { type: papermc, project: paper, version: "1.21.4" } }
plugins:
  - { jarId: lp, name: LuckPerms.jar, download: { type: luckperms, platform: bukkit } }
  - { jarId: via, name: ViaVersion.jar, download: { type: github, repository: ViaVersion/ViaVersion, asset: 'ViaVersion-.*\.jar' } }
  - { jarId: core, name: core.jar, download: { type: maven, source: nexus, groupId: g, artifactId: core, version: latest-snapshot } }
  - { jarId: fawe, name: fawe.jar, download: { type: modrinth, project: fawe, loaders: [paper] } }
  - { jarId: x, name: x.jar, url: https://example.com/x.jar, sha256: d72941fbfffa8f3e7bf29f5b94ac29bcb20dd042b2ab2e5dbfb06ac12afa456a }
  - { jarId: y, name: y.jar, source: static, path: y.jar }
worlds:
  - { name: world, source: maps, path: lobby.zip, extract: zip, stripComponents: 1 }
patches:
  - file: server.properties
    fields: [{ field: max-players, value: 20 }, { field: online-mode, value: "${ONLINE}", type: bool }]
`,
		},
		{name: "unknown key", yaml: "version: 1\nid: a\nplugin: []\n", want: "field plugin not found"},
		{name: "version", yaml: "version: 2\n", want: "version: must be 1"},
		{name: "two forms", yaml: "version: 1\nserver: { url: https://e.com/a.jar, source: s, path: a.jar }\n", want: "exactly one of"},
		{name: "no form", yaml: "version: 1\nplugins: [{ jarId: a, name: a.jar }]\n", want: "exactly one of"},
		{name: "bad name", yaml: "version: 1\nplugins: [{ jarId: a, name: a.zip, url: https://e.com/a }]\n", want: "must end with .jar"},
		{name: "path escape", yaml: "version: 1\nplugins: [{ jarId: a, name: a.jar, source: s, path: ../a.jar }]\n", want: "must not leave"},
		{name: "duplicate", yaml: "version: 1\nplugins: [{ jarId: a, name: a.jar, url: https://e.com/a }, { jarId: a, name: b.jar, url: https://e.com/b }]\n", want: "declared twice"},
		{name: "missing field", yaml: "version: 1\nserver: { download: { type: maven, source: s, groupId: g } }\n", want: "artifactId is required"},
		{name: "foreign field", yaml: "version: 1\nserver: { download: { type: papermc, project: paper, platform: bukkit } }\n", want: "platform is not supported"},
		{name: "unknown type", yaml: "version: 1\nserver: { download: { type: spigot } }\n", want: "unknown type"},
		{name: "credentials over http", yaml: "version: 1\nserver: { download: { type: maven, repository: http://e.com/m, credentials: c, groupId: g, artifactId: a, version: '1' } }\n", want: "only sent over https"},
		{name: "userinfo", yaml: "version: 1\nserver: { url: 'https://u:p@e.com/a.jar' }\n", want: "must not be embedded"},
		{name: "bad sha", yaml: "version: 1\nserver: { url: https://e.com/a.jar, sha256: abc }\n", want: "64 hexadecimal"},
		{name: "patch type", yaml: "version: 1\npatches: [{ file: a.toml, fields: [{ field: a, value: b }] }]\n", want: "cannot be patched"},
		{name: "patch value", yaml: "version: 1\npatches: [{ file: a.yml, fields: [{ field: a, value: [1] }] }]\n", want: "value must be"},
		{name: "remove with fields", yaml: "version: 1\nplugins: [{ jarId: a, remove: true, name: a.jar }]\n", want: "remove: true takes no other field"},
		{name: "strip without extract", yaml: "version: 1\nworlds: [{ name: w, source: s, path: w, stripComponents: 1 }]\n", want: "stripComponents requires extract"},
		{name: "bad regex", yaml: "version: 1\nserver: { download: { type: github, repository: a/b, asset: '(' } }\n", want: "asset:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tpl := &Template{}
			err := yamlutil.Decode([]byte(tt.yaml), tpl)
			if err == nil {
				err = tpl.Validate()
			}
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("expected an error containing %q, got %v", tt.want, err)
			}
		})
	}
}
