package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func archive(t *testing.T, files map[string][]byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for n, b := range files {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o755, Size: int64(len(b)), Typeflag: tar.TypeReg})
		tw.Write(b)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func release(t *testing.T, tag string, bin []byte, corrupt bool) (*Updater, func()) {
	u := New("o/r")
	u.GOOS, u.GOARCH = "linux", "amd64"
	arc := archive(t, map[string][]byte{"scout": bin, "README.md": []byte("hi")})
	sum := sha256.Sum256(arc)
	if corrupt {
		sum[0] ^= 0xff
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/releases/latest":
			fmt.Fprintf(w, `{"tag_name":%q}`, tag)
		case strings.HasSuffix(r.URL.Path, "/"+u.ArchiveName(tag)):
			w.Write(arc)
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), u.ArchiveName(tag))
		default:
			http.NotFound(w, r)
		}
	}))
	u.APIBase, u.DownloadBase = srv.URL, srv.URL
	return u, srv.Close
}

func TestLatestAndFetchVerified(t *testing.T) {
	bin := bytes.Repeat([]byte("B"), 4096)
	u, stop := release(t, "v1.2.3", bin, false)
	defer stop()
	tag, err := u.Latest(context.Background())
	if err != nil || tag != "v1.2.3" {
		t.Fatalf("%q %v", tag, err)
	}
	got, err := u.Fetch(context.Background(), tag)
	if err != nil || !bytes.Equal(got, bin) {
		t.Fatalf("%v", err)
	}
}

func TestChecksumMismatchIsRefused(t *testing.T) {
	u, stop := release(t, "v1.0.0", bytes.Repeat([]byte("B"), 4096), true)
	defer stop()
	if _, err := u.Fetch(context.Background(), "v1.0.0"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered download must be refused: %v", err)
	}
}

func TestMissingReleaseAndUnsupportedOS(t *testing.T) {
	u, stop := release(t, "v1.0.0", bytes.Repeat([]byte("B"), 4096), false)
	defer stop()
	if _, err := u.Fetch(context.Background(), "v9.9.9"); err == nil {
		t.Fatal("missing asset must fail")
	}
	u.GOOS = "windows"
	if _, err := u.Fetch(context.Background(), "v1.0.0"); err == nil || !strings.Contains(err.Error(), "no prebuilt release") {
		t.Fatalf("%v", err)
	}
}

func TestArchiveWithoutBinaryIsRejected(t *testing.T) {
	if _, err := extract(archive(t, map[string][]byte{"../../evil": bytes.Repeat([]byte("x"), 2000), "README.md": []byte("x")})); err == nil {
		t.Fatal("must not accept an archive lacking the scout entry")
	}
}

func TestReplaceIsAtomicAndKeepsExecutable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "scout")
	os.WriteFile(p, []byte("old"), 0o755)
	if err := Replace(p, []byte("new binary")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	st, _ := os.Stat(p)
	if string(b) != "new binary" || st.Mode().Perm() != 0o755 {
		t.Fatalf("%q %v", b, st.Mode())
	}
	if m, _ := filepath.Glob(filepath.Join(dir, ".scout-update-*")); len(m) != 0 {
		t.Fatalf("temp files left behind: %v", m)
	}
	if err := Replace(filepath.Join(dir, "nope", "scout"), []byte("x")); err == nil {
		t.Fatal("unwritable target must error")
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		cur, lat string
		want     bool
	}{
		{"v0.1.2", "v0.1.3", true}, {"v0.1.2", "v0.1.2", false}, {"v0.2.0", "v0.1.9", false},
		{"v0.9.0", "v0.10.0", true}, {"dev", "v0.1.0", true}, {"v0.1.2-3-gabc", "v0.1.2", true}, {"v1.0.0", "garbage", false},
	}
	for _, c := range cases {
		if got := Newer(c.cur, c.lat); got != c.want {
			t.Errorf("Newer(%q,%q)=%v want %v", c.cur, c.lat, got, c.want)
		}
	}
}
