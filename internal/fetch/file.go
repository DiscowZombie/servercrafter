package fetch

import (
	"archive/zip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Checksums maps an algorithm (sha1, sha256, sha512) to the expected hexadecimal digest
type Checksums map[string]string

// Add records an expected digest, failing when another one was already expected for the algorithm
func (c Checksums) Add(algorithm string, digest string) error {
	algorithm = strings.ToLower(algorithm)
	digest = strings.ToLower(strings.TrimSpace(digest))
	if digest == "" {
		return nil
	}
	if existing, ok := c[algorithm]; ok && existing != digest {
		return fmt.Errorf("conflicting %s checksums: %s and %s", algorithm, existing, digest)
	}
	c[algorithm] = digest
	return nil
}

func newHash(algorithm string) (hash.Hash, error) {
	switch algorithm {
	case "sha1":
		return sha1.New(), nil
	case "sha256":
		return sha256.New(), nil
	case "sha512":
		return sha512.New(), nil
	}
	return nil, fmt.Errorf("unsupported checksum algorithm %s", algorithm)
}

// ChecksumError is a downloaded file not matching its expected digest
type ChecksumError struct {
	Algorithm, Expected, Actual string
}

func (e *ChecksumError) Error() string {
	return fmt.Sprintf("%s mismatch: expected %s, got %s", e.Algorithm, e.Expected, e.Actual)
}

// WriteFile downloads the content open returns to dest atomically: it is written next to dest,
// verified against checks, then renamed over dest. Nothing is left behind on failure. The SHA-256
// of the content is returned whether or not it was expected.
func WriteFile(ctx context.Context, dest string, open Opener, checks Checksums) (string, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".part-*")
	if err != nil {
		return "", err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	content, err := open(ctx)
	if err != nil {
		return "", err
	}
	defer content.Close()

	hashes := map[string]hash.Hash{"sha256": sha256.New()}
	for algorithm := range checks {
		if _, ok := hashes[algorithm]; !ok {
			if hashes[algorithm], err = newHash(algorithm); err != nil {
				return "", err
			}
		}
	}
	writers := []io.Writer{tmp}
	for _, h := range hashes {
		writers = append(writers, h)
	}
	if _, err = io.Copy(io.MultiWriter(writers...), &contextReader{ctx: ctx, r: content}); err != nil {
		return "", err
	}
	if err = tmp.Close(); err != nil {
		return "", err
	}
	for _, algorithm := range sortedAlgorithms(checks) {
		actual := hex.EncodeToString(hashes[algorithm].Sum(nil))
		if actual != checks[algorithm] {
			return "", &ChecksumError{Algorithm: algorithm, Expected: checks[algorithm], Actual: actual}
		}
	}
	if err = os.Chmod(tmp.Name(), 0o644); err != nil {
		return "", err
	}
	if err = os.Rename(tmp.Name(), dest); err != nil {
		return "", err
	}
	return hex.EncodeToString(hashes["sha256"].Sum(nil)), nil
}

func sortedAlgorithms(checks Checksums) []string {
	algorithms := make([]string, 0, len(checks))
	for algorithm := range checks {
		algorithms = append(algorithms, algorithm)
	}
	sort.Strings(algorithms)
	return algorithms
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// FileSHA256 returns the SHA-256 of a file
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Unzip extracts archive into dest, dropping the first strip components of every entry. Entries
// escaping dest and symbolic links are rejected.
func Unzip(archive string, dest string, strip int) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer reader.Close()
	if err = os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	for _, entry := range reader.File {
		name := path.Clean(strings.ReplaceAll(entry.Name, `\`, "/"))
		parts := strings.Split(strings.TrimPrefix(name, "/"), "/")
		if len(parts) <= strip {
			continue
		}
		rel := path.Join(parts[strip:]...)
		if rel == "." {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, "../") || path.IsAbs(rel) {
			return fmt.Errorf("zip entry %q escapes the extraction directory", entry.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		mode := entry.Mode()
		switch {
		case mode&fs.ModeSymlink != 0:
			return fmt.Errorf("zip entry %q is a symbolic link, refusing to extract it", entry.Name)
		case entry.FileInfo().IsDir():
			if err = os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		default:
			if err = extractEntry(entry, target); err != nil {
				return fmt.Errorf("extract %s: %w", entry.Name, err)
			}
		}
	}
	return nil
}

func extractEntry(entry *zip.File, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	src, err := entry.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	perm := os.FileMode(0o644)
	if entry.Mode()&0o111 != 0 {
		perm = 0o755
	}
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err = io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}

// CopyDir copies the content of src into dst, overwriting the files both hold
func CopyDir(src string, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", src)
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			if d.Name() == ".git" && p != src {
				return filepath.SkipDir
			}
			return os.MkdirAll(target, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err = os.RemoveAll(target); err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.Type().IsRegular():
			return CopyFile(p, target)
		}
		return nil
	})
}

// CopyFile copies a regular file, keeping whether it is executable
func CopyFile(src string, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if info.Mode()&0o111 != 0 {
		perm = 0o755
	}
	if err = os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// A previous build may have left a symbolic link or a directory in the way
	if existing, err := os.Lstat(dst); err == nil && !existing.Mode().IsRegular() {
		if err = os.RemoveAll(dst); err != nil {
			return err
		}
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
