package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		latest, running string
		want            bool
	}{
		{"v0.6.0", "v0.5.0", true},
		{"0.6.0", "0.5.0", true},
		{"v0.5.0", "v0.5.0", false},
		{"v0.5.0", "v0.6.0", false},
		{"v1.0.0", "v0.9.9", true},
		{"v0.5.1", "v0.5.0", true},
		{"v0.10.0", "v0.9.0", true}, // numeric, not lexicographic
		{"v0.5.0", "v0.4.0-52-g3395732", true},
		{"v0.5.0", "v0.5.0-3-gabc1234", false}, // dev build ahead of release
		{"v0.5.0", "v0.5.0-3-gabc1234-dirty", false},
		{"v0.5.0", "dev", false},      // unparseable running never claims update
		{"garbage", "v0.5.0", false},  // unparseable latest never claims update
		{"", "", false},
	}
	for _, c := range cases {
		if got := Newer(c.latest, c.running); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.latest, c.running, got, c.want)
		}
	}
}

func TestIntervalDuration(t *testing.T) {
	if d, ok := IntervalDuration("daily"); !ok || d != 24*time.Hour {
		t.Errorf("daily = (%v, %v)", d, ok)
	}
	if d, ok := IntervalDuration("weekly"); !ok || d != 7*24*time.Hour {
		t.Errorf("weekly = (%v, %v)", d, ok)
	}
	if d, ok := IntervalDuration("monthly"); !ok || d != 30*24*time.Hour {
		t.Errorf("monthly = (%v, %v)", d, ok)
	}
	if _, ok := IntervalDuration("off"); ok {
		t.Error("off should disable")
	}
	// Unknown values fall back to the default cadence, never to off.
	if d, ok := IntervalDuration("bogus"); !ok || d != 30*24*time.Hour {
		t.Errorf("bogus = (%v, %v), want monthly fallback", d, ok)
	}
}

func TestAssetSelection(t *testing.T) {
	rel := &Release{
		TagName: "v0.5.0",
		Assets: []Asset{
			{Name: "hoptrail_0.5.0_checksums.txt"},
			{Name: "hoptrail_0.5.0_linux_amd64.tar.gz"},
			{Name: "hoptrail_0.5.0_linux_arm64.tar.gz"},
		},
	}
	if a, ok := rel.BinaryAsset("amd64"); !ok || a.Name != "hoptrail_0.5.0_linux_amd64.tar.gz" {
		t.Errorf("BinaryAsset(amd64) = (%v, %v)", a.Name, ok)
	}
	if a, ok := rel.BinaryAsset("arm64"); !ok || a.Name != "hoptrail_0.5.0_linux_arm64.tar.gz" {
		t.Errorf("BinaryAsset(arm64) = (%v, %v)", a.Name, ok)
	}
	if _, ok := rel.BinaryAsset("riscv64"); ok {
		t.Error("BinaryAsset(riscv64) should miss")
	}
	if a, ok := rel.ChecksumsAsset(); !ok || a.Name != "hoptrail_0.5.0_checksums.txt" {
		t.Errorf("ChecksumsAsset = (%v, %v)", a.Name, ok)
	}
	if v := rel.Version(); v != "0.5.0" {
		t.Errorf("Version = %q", v)
	}
}

// fakeELF is a minimal payload passing the \x7fELF shape check.
var fakeELF = append([]byte("\x7fELF"), bytes.Repeat([]byte{0x42}, 256)...)

// buildTarball produces a gzipped tar with one member.
func buildTarball(t *testing.T, member string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: member, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// fakeGitHub serves /repos/<repo>/releases/latest plus asset downloads.
type fakeGitHub struct {
	srv      *httptest.Server
	tarball  []byte
	sums     string
	relJSON  func(base string) string
	statusOn map[string]int // path → forced status
}

func newFakeGitHub(t *testing.T, tarball []byte, sums string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{tarball: tarball, sums: sums, statusOn: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if code := f.statusOn[r.URL.Path]; code != 0 {
			w.WriteHeader(code)
			return
		}
		base := "http://" + r.Host
		fmt.Fprintf(w, `{
			"tag_name": "v0.6.0",
			"html_url": "%s/releases/v0.6.0",
			"assets": [
				{"name": "hoptrail_0.6.0_checksums.txt", "browser_download_url": "%s/dl/checksums", "size": %d},
				{"name": "hoptrail_0.6.0_linux_amd64.tar.gz", "browser_download_url": "%s/dl/tarball", "size": %d}
			]
		}`, base, base, len(f.sums), base, len(f.tarball))
	})
	mux.HandleFunc("/dl/checksums", func(w http.ResponseWriter, r *http.Request) {
		if code := f.statusOn[r.URL.Path]; code != 0 {
			w.WriteHeader(code)
			return
		}
		io.WriteString(w, f.sums)
	})
	mux.HandleFunc("/dl/tarball", func(w http.ResponseWriter, r *http.Request) {
		w.Write(f.tarball)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) client() *Client {
	return &Client{APIBase: f.srv.URL, Repo: "owner/repo", HTTP: f.srv.Client()}
}

func sumLine(name string, content []byte) string {
	h := sha256.Sum256(content)
	return hex.EncodeToString(h[:]) + "  " + name + "\n"
}

func TestLatest(t *testing.T) {
	tarball := buildTarball(t, "hoptrail", fakeELF)
	f := newFakeGitHub(t, tarball, sumLine("hoptrail_0.6.0_linux_amd64.tar.gz", tarball))
	rel, err := f.client().Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.TagName != "v0.6.0" || len(rel.Assets) != 2 {
		t.Errorf("Latest = %+v", rel)
	}
}

func TestLatest_NoReleases(t *testing.T) {
	f := newFakeGitHub(t, nil, "")
	f.statusOn["/repos/owner/repo/releases/latest"] = http.StatusNotFound
	if _, err := f.client().Latest(context.Background()); err == nil {
		t.Fatal("want error on 404")
	}
}

func TestDownloadBinary(t *testing.T) {
	tarball := buildTarball(t, "hoptrail", fakeELF)
	f := newFakeGitHub(t, tarball, sumLine("hoptrail_0.6.0_linux_amd64.tar.gz", tarball))
	c := f.client()
	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "update", "hoptrail")
	if err := c.DownloadBinary(context.Background(), rel, "amd64", dest); err != nil {
		t.Fatalf("DownloadBinary: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	if !bytes.Equal(got, fakeELF) {
		t.Error("staged binary content mismatch")
	}
	fi, _ := os.Stat(dest)
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("staged mode = %v, want 0755", fi.Mode().Perm())
	}
	// The temp tarball must not linger next to the destination.
	entries, _ := os.ReadDir(filepath.Dir(dest))
	if len(entries) != 1 {
		t.Errorf("staging dir has %d entries, want just the binary", len(entries))
	}
}

func TestDownloadBinary_ChecksumMismatch(t *testing.T) {
	tarball := buildTarball(t, "hoptrail", fakeELF)
	f := newFakeGitHub(t, tarball, sumLine("hoptrail_0.6.0_linux_amd64.tar.gz", []byte("other bytes")))
	c := f.client()
	rel, _ := c.Latest(context.Background())
	dest := filepath.Join(t.TempDir(), "hoptrail")
	err := c.DownloadBinary(context.Background(), rel, "amd64", dest)
	if err == nil {
		t.Fatal("want checksum error")
	}
	if _, serr := os.Stat(dest); serr == nil {
		t.Error("failed download must not land at destination")
	}
}

func TestDownloadBinary_NoArchAsset(t *testing.T) {
	tarball := buildTarball(t, "hoptrail", fakeELF)
	f := newFakeGitHub(t, tarball, sumLine("hoptrail_0.6.0_linux_amd64.tar.gz", tarball))
	c := f.client()
	rel, _ := c.Latest(context.Background())
	err := c.DownloadBinary(context.Background(), rel, "riscv64", filepath.Join(t.TempDir(), "hoptrail"))
	if err == nil {
		t.Fatal("want no-asset error")
	}
}

func TestDownloadBinary_MissingChecksumEntry(t *testing.T) {
	tarball := buildTarball(t, "hoptrail", fakeELF)
	f := newFakeGitHub(t, tarball, sumLine("some_other_file.tar.gz", tarball))
	c := f.client()
	rel, _ := c.Latest(context.Background())
	err := c.DownloadBinary(context.Background(), rel, "amd64", filepath.Join(t.TempDir(), "hoptrail"))
	if err == nil {
		t.Fatal("want missing-entry error")
	}
}

func TestDownloadBinary_TarballWithoutBinary(t *testing.T) {
	tarball := buildTarball(t, "README.md", []byte("not a binary"))
	f := newFakeGitHub(t, tarball, sumLine("hoptrail_0.6.0_linux_amd64.tar.gz", tarball))
	c := f.client()
	rel, _ := c.Latest(context.Background())
	err := c.DownloadBinary(context.Background(), rel, "amd64", filepath.Join(t.TempDir(), "hoptrail"))
	if err == nil {
		t.Fatal("want no-member error")
	}
}

func TestDownloadBinary_NonELFMember(t *testing.T) {
	tarball := buildTarball(t, "hoptrail", []byte("#!/bin/sh\necho not an ELF\n"))
	f := newFakeGitHub(t, tarball, sumLine("hoptrail_0.6.0_linux_amd64.tar.gz", tarball))
	c := f.client()
	rel, _ := c.Latest(context.Background())
	err := c.DownloadBinary(context.Background(), rel, "amd64", filepath.Join(t.TempDir(), "hoptrail"))
	if err == nil {
		t.Fatal("want non-ELF error")
	}
}

// ---------- checker ----------

type memStore struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemStore() *memStore { return &memStore{m: map[string]string{}} }

func (s *memStore) GetConfig(_ context.Context, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	return v, ok, nil
}

func (s *memStore) SetConfig(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = value
	return nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestChecker_FirstPassChecksImmediately(t *testing.T) {
	store := newMemStore()
	calls := 0
	c := &Checker{
		Store: store,
		Fetch: func(context.Context) (*Release, error) {
			calls++
			return &Release{TagName: "v0.6.0", HTMLURL: "https://example.test/r"}, nil
		},
		Log: testLogger(),
		now: func() time.Time { return time.UnixMilli(1_000_000) },
	}
	c.pass(context.Background())
	if calls != 1 {
		t.Fatalf("fetch calls = %d, want 1 (no prior check → due)", calls)
	}
	lc := c.ReadLastCheck(context.Background())
	if lc == nil || lc.LatestVersion != "0.6.0" || lc.At != 1_000_000 {
		t.Errorf("persisted = %+v", lc)
	}
}

func TestChecker_RespectsInterval(t *testing.T) {
	store := newMemStore()
	store.SetConfig(context.Background(), KeyCheckInterval, "daily")
	now := time.Unix(100_000_000, 0)
	calls := 0
	c := &Checker{
		Store: store,
		Fetch: func(context.Context) (*Release, error) {
			calls++
			return &Release{TagName: "v0.6.0"}, nil
		},
		Log: testLogger(),
		now: func() time.Time { return now },
	}
	c.pass(context.Background()) // first: due
	c.pass(context.Background()) // immediately again: not due
	if calls != 1 {
		t.Fatalf("fetch calls = %d, want 1", calls)
	}
	now = now.Add(23 * time.Hour)
	c.pass(context.Background())
	if calls != 1 {
		t.Fatalf("fetch calls after 23h = %d, want still 1", calls)
	}
	now = now.Add(2 * time.Hour) // 25h total
	c.pass(context.Background())
	if calls != 2 {
		t.Fatalf("fetch calls after 25h = %d, want 2", calls)
	}
}

func TestChecker_OffNeverChecks(t *testing.T) {
	store := newMemStore()
	store.SetConfig(context.Background(), KeyCheckInterval, "off")
	calls := 0
	c := &Checker{
		Store: store,
		Fetch: func(context.Context) (*Release, error) { calls++; return nil, nil },
		Log:   testLogger(),
	}
	c.pass(context.Background())
	if calls != 0 {
		t.Fatalf("fetch calls = %d, want 0 with interval off", calls)
	}
}

func TestChecker_PersistsErrors(t *testing.T) {
	store := newMemStore()
	c := &Checker{
		Store: store,
		Fetch: func(context.Context) (*Release, error) {
			return nil, fmt.Errorf("github unreachable")
		},
		Log: testLogger(),
		now: func() time.Time { return time.UnixMilli(5_000) },
	}
	got := c.CheckNow(context.Background())
	if got.Err == "" || got.LatestVersion != "" {
		t.Errorf("CheckNow = %+v, want error recorded", got)
	}
	lc := c.ReadLastCheck(context.Background())
	if lc == nil || lc.Err != "github unreachable" {
		t.Errorf("persisted = %+v", lc)
	}
}
