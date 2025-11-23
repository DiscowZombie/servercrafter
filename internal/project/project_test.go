package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, content string) (*Project, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), DefaultFile)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestLoadValid(t *testing.T) {
	p, err := load(t, `
version: 1
templatesDir: tpl
credentials:
  gitlab: { type: header, name: PRIVATE-TOKEN, value: { env: GITLAB_TOKEN } }
  nexus: { type: basic, username: { env: NEXUS_USER }, password: { file: secrets/nexus } }
  files: { type: basic, username: { value: builder }, password: { env: FILES_PASSWORD } }
  ssh: { type: ssh, privateKey: { file: ~/.ssh/id_ed25519 } }
  s3: { type: s3, accessKey: { env: AK }, secretKey: { env: SK }, region: fr-par }
sources:
  static: { type: local, directory: static }
  nexus: { type: nexus, baseUrl: https://nexus.example.com, repository: maven-releases, credentials: nexus }
  gl: { type: gitlab, baseUrl: https://gitlab.example.com, group: mc, credentials: gitlab }
  bucket: { type: s3, baseUrl: https://s3.fr-par.scw.cloud/maps/, credentials: s3 }
  maps: { type: git, repository: git@gitlab.example.com:mc/maps.git, ref: main, credentials: ssh }
`)
	if err != nil {
		t.Fatal(err)
	}
	if p.TemplatesPath() != filepath.Join(p.Dir, "tpl") {
		t.Errorf("templates path: %s", p.TemplatesPath())
	}
}

func TestLoadInvalid(t *testing.T) {
	tests := map[string]struct{ yaml, want string }{
		"plain secret":       {"version: 1\ncredentials: { c: { type: basic, username: admin, password: { env: P } } }\n", "cannot unmarshal"},
		"both env and file":  {"version: 1\ncredentials: { c: { type: bearer, token: { env: A, file: b } } }\n", "exactly one of env, file or value"},
		"plain password":     {"version: 1\ncredentials: { c: { type: basic, username: { value: u }, password: { value: p } } }\n", "password: a secret cannot be written"},
		"plain token":        {"version: 1\ncredentials: { c: { type: bearer, token: { value: t } } }\n", "token: a secret cannot be written"},
		"unknown credential": {"version: 1\nsources: { s: { type: http, baseUrl: https://e.com, credentials: nope } }\n", "unknown credential"},
		"http credentials":   {"version: 1\ncredentials: { c: { type: bearer, token: { env: T } } }\nsources: { s: { type: http, baseUrl: http://e.com, credentials: c } }\n", "only sent over https"},
		"wrong cred type":    {"version: 1\ncredentials: { c: { type: ssh, privateKey: { env: K } } }\nsources: { s: { type: http, baseUrl: https://e.com, credentials: c } }\n", "cannot use a ssh credential"},
		"missing field":      {"version: 1\nsources: { s: { type: local } }\n", "directory is required"},
		"unknown type":       {"version: 1\nsources: { s: { type: ftp } }\n", "unknown type"},
		"unknown key":        {"version: 1\nsource: {}\n", "field source not found"},
		"bad name":           {"version: 1\nsources: { 'a b': { type: local, directory: x } }\n", "invalid name"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, tt.yaml)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected an error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestValueRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secret"), []byte("s3cr3t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := (&Value{File: "secret"}).Read(dir); err != nil || v != "s3cr3t" {
		t.Errorf("file: %q, %v", v, err)
	}
	t.Setenv("SERVERCRAFTER_TEST_SECRET", "abc")
	if v, err := (&Value{Env: "SERVERCRAFTER_TEST_SECRET"}).Read(dir); err != nil || v != "abc" {
		t.Errorf("env: %q, %v", v, err)
	}
	if v, err := (&Value{Plain: "builder"}).Read(dir); err != nil || v != "builder" {
		t.Errorf("value: %q, %v", v, err)
	}
	if _, err := (&Value{Env: "SERVERCRAFTER_TEST_UNSET"}).Read(dir); err == nil || !strings.Contains(err.Error(), "not set") {
		t.Errorf("unset env: %v", err)
	}
}
