// Package update implements `scout update`: find the latest GitHub release,
// download the archive for this OS/architecture, verify its SHA-256 against the
// release's checksums.txt, and atomically replace the running executable.
package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	maxArchive = 100 << 20
	maxBinary  = 200 << 20
)

type Updater struct {
	Repo         string // owner/name
	APIBase      string // default https://api.github.com
	DownloadBase string // default https://github.com
	Client       *http.Client
	GOOS, GOARCH string
}

func New(repo string) *Updater {
	return &Updater{Repo: repo, APIBase: "https://api.github.com", DownloadBase: "https://github.com",
		Client: &http.Client{Timeout: 2 * time.Minute}, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
}

// Latest returns the tag of the newest published release.
func (u *Updater) Latest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/releases/latest", u.APIBase, u.Repo), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "scout-updater")
	resp, err := u.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("checking for updates: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", errors.New("no published release found")
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return "", errors.New("GitHub rate limit reached; try again later")
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("GitHub returned %s", resp.Status)
	}
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil || rel.Tag == "" {
		return "", errors.New("unexpected response from GitHub")
	}
	return rel.Tag, nil
}

func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "scout-updater")
	resp, err := u.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: download too large", url)
	}
	return b, nil
}

// ArchiveName matches install.sh / install.ps1 and the release assets
// (.zip on Windows, .tar.gz elsewhere; see .goreleaser.yaml).
func (u *Updater) ArchiveName(tag string) string {
	ext := "tar.gz"
	if u.GOOS == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("scoutclaw_%s_%s_%s.%s", strings.TrimPrefix(tag, "v"), u.GOOS, u.GOARCH, ext)
}

// Fetch downloads and verifies the release for this platform and returns the new binary.
func (u *Updater) Fetch(ctx context.Context, tag string) ([]byte, error) {
	if u.GOOS != "linux" && u.GOOS != "darwin" && u.GOOS != "windows" {
		return nil, fmt.Errorf("no prebuilt release for %s; build from source", u.GOOS)
	}
	name := u.ArchiveName(tag)
	base := fmt.Sprintf("%s/%s/releases/download/%s", u.DownloadBase, u.Repo, tag)
	archive, err := u.get(ctx, base+"/"+name, maxArchive)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", name, err)
	}
	sums, err := u.get(ctx, base+"/checksums.txt", 1<<20)
	if err != nil {
		return nil, fmt.Errorf("downloading checksums: %w", err)
	}
	want := checksumFor(string(sums), name)
	if want == "" {
		return nil, fmt.Errorf("%s is not listed in checksums.txt", name)
	}
	got := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(got[:]), want) {
		return nil, errors.New("checksum mismatch: refusing to install")
	}
	if u.GOOS == "windows" {
		return extractZip(archive)
	}
	return extract(archive)
}

func checksumFor(sums, name string) string {
	for _, line := range strings.Split(sums, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return f[0]
		}
	}
	return ""
}

// extract returns the "scout" file from the archive; no other entry is touched.
func extract(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("bad archive: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("archive does not contain the scout binary")
		}
		if err != nil {
			return nil, fmt.Errorf("bad archive: %w", err)
		}
		if h.Typeflag == tar.TypeReg && h.Name == "scout" {
			b, err := io.ReadAll(io.LimitReader(tr, maxBinary+1))
			if err != nil || int64(len(b)) > maxBinary || len(b) < 1024 {
				return nil, errors.New("archive contains an invalid scout binary")
			}
			return b, nil
		}
	}
}

// extractZip returns the "scout.exe" file from a Windows release archive.
func extractZip(archive []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("bad archive: %w", err)
	}
	for _, f := range zr.File {
		if f.Name != "scout.exe" || !f.Mode().IsRegular() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("bad archive: %w", err)
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, maxBinary+1))
		if err != nil || int64(len(b)) > maxBinary || len(b) < 1024 {
			return nil, errors.New("archive contains an invalid scout binary")
		}
		return b, nil
	}
	return nil, errors.New("archive does not contain the scout binary")
}

// Replace atomically swaps the executable at path with data. Renaming over a
// running binary is safe on Linux and macOS: the old process keeps its inode.
// Windows refuses to overwrite a running executable but allows renaming it,
// so there the old file is moved aside to path+".old" first (and removed on
// the next update, once it is no longer running).
func Replace(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".scout-update-*")
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%s is not writable by you; reinstall with the install script or run with sufficient permissions: %w", dir, err)
		}
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return os.Rename(tmp.Name(), path)
	}
	old := path + ".old"
	_ = os.Remove(old) // left by the previous update
	if err := os.Rename(path, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("moving the running executable aside: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Rename(old, path)
		return err
	}
	return nil
}

// Newer reports whether latest is a higher vX.Y.Z than current. Anything that
// is not a clean version (e.g. "dev", "v0.1.2-3-gabc") counts as older.
func Newer(current, latest string) bool {
	c, cok := parse(current)
	l, lok := parse(latest)
	if !lok {
		return false
	}
	if !cok {
		return true
	}
	for i := range c {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func parse(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
