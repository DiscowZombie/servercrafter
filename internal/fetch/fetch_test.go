package fetch

import (
	"archive/zip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

const helloSHA256 = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"

func testClient() *Client {
	log := logrus.New()
	log.SetOutput(os.Stderr)
	c := NewClient("servercrafter-test", log)
	c.Backoff = time.Millisecond
	return c
}

func TestWriteFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hello":
			_, _ = w.Write([]byte("hello"))
		default:
			http.Error(w, "Asset search returned no results", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := testClient()
	dir := t.TempDir()
	dest := filepath.Join(dir, "plugins", "a.jar")

	sum, err := WriteFile(context.Background(), dest, client.HTTPOpener(server.URL+"/hello", nil, nil), Checksums{"sha256": helloSHA256})
	if err != nil || sum != helloSHA256 {
		t.Fatalf("download: %s, %v", sum, err)
	}

	// An error response must never end up installed as the artifact
	missing := filepath.Join(dir, "plugins", "b.jar")
	_, err = WriteFile(context.Background(), missing, client.HTTPOpener(server.URL+"/missing", nil, nil), nil)
	if !IsNotFound(err) {
		t.Fatalf("expected a 404, got %v", err)
	}
	if _, err = os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed download left %s behind", missing)
	}

	// Neither must content not matching its checksum, and the previous file must survive
	_, err = WriteFile(context.Background(), dest, client.HTTPOpener(server.URL+"/hello", nil, nil), Checksums{"sha1": strings.Repeat("0", 40)})
	var mismatch *ChecksumError
	if !errors.As(err, &mismatch) {
		t.Fatalf("expected a checksum error, got %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "plugins"))
	if len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}

func TestRetries(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	body, err := testClient().GetBytes(context.Background(), server.URL, nil, 10)
	if err != nil || string(body) != "ok" || calls.Load() != 3 {
		t.Fatalf("got %q, %v after %d calls", body, err, calls.Load())
	}
}

func TestCredentialsNotForwardedAcrossHosts(t *testing.T) {
	var leaked atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("PRIVATE-TOKEN") + r.Header.Get("Authorization"))
		_, _ = w.Write([]byte("ok"))
	}))
	defer other.Close()
	// 127.0.0.1 and localhost are different hosts for the client
	otherURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otherURL, http.StatusFound)
	}))
	defer origin.Close()
	client := testClient()
	for _, auth := range []Auth{HeaderAuth{Name: "PRIVATE-TOKEN", Value: "secret"}, BasicAuth{Username: "u", Password: "secret"}} {
		if _, err := client.GetBytes(context.Background(), origin.URL, auth, 10); err != nil {
			t.Fatal(err)
		}
		if v := leaked.Load().(string); v != "" {
			t.Errorf("credentials forwarded to another host: %q", v)
		}
	}
}

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for name, content := range files {
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = entry.Write([]byte(content))
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

func TestUnzip(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "world.zip")
	writeZip(t, archive, map[string]string{"lobby/level.dat": "dat", "lobby/region/r.0.0.mca": "mca"})
	dest := filepath.Join(dir, "out")
	if err := Unzip(archive, dest, 1); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dest, "region", "r.0.0.mca")); err != nil || string(data) != "mca" {
		t.Errorf("extracted: %q, %v", data, err)
	}

	evil := filepath.Join(dir, "evil.zip")
	writeZip(t, evil, map[string]string{"../../escape": "x"})
	if err := Unzip(evil, filepath.Join(dir, "evil"), 0); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Errorf("zip slip: %v", err)
	}
}

func TestCopyDirOverwrites(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	_ = os.MkdirAll(filepath.Join(src, "plugins", "LuckPerms"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "plugins", "LuckPerms", "config.yml"), []byte("new"), 0o644)
	_ = os.MkdirAll(filepath.Join(dst, "plugins", "LuckPerms"), 0o755)
	_ = os.WriteFile(filepath.Join(dst, "plugins", "LuckPerms", "config.yml"), []byte("old"), 0o644)
	if err := CopyDir(src, dst); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "plugins", "LuckPerms", "config.yml")); string(data) != "new" {
		t.Errorf("got %q", data)
	}
	if err := CopyDir(src, dst); err != nil {
		t.Fatalf("second copy: %v", err)
	}
}
