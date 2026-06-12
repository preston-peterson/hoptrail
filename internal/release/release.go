// Package release talks to GitHub Releases for the self-update path
// (v0.5 design §6 C-remote, shipped post-publish): find the latest
// published release, decide whether it's newer than the running build,
// and download the architecture-matched prebuilt binary with sha256
// verification against the release's checksums asset.
//
// The package is dependency-free of the rest of hoptrail — it takes
// primitives and a destination path, and the server layer stages the
// result where the existing apply path (internal/server/update.go)
// already looks.
package release

import (
	"archive/tar"
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
	"strconv"
	"strings"
	"time"
)

// DefaultRepo is the canonical source of hoptrail releases.
const DefaultRepo = "preston-peterson/hoptrail"

// maxDownloadBytes caps both the compressed tarball and the extracted
// binary — same bound as the UI upload path (real binary ~10 MB
// compressed; 100 MB leaves debug-build headroom while bounding abuse
// from a compromised or misconfigured source).
const maxDownloadBytes = 100 << 20

// Config keys shared by the server endpoints and the background
// checker. Both live in the config KV table like every other
// UI-managed setting.
const (
	// KeyCheckInterval holds "off" | "daily" | "weekly" | "monthly".
	// Absent or unrecognized falls back to DefaultCheckInterval.
	KeyCheckInterval = "update.check_interval"
	// KeyLastCheck holds a JSON-encoded LastCheck — the most recent
	// check result, manual or automatic, surviving restarts.
	KeyLastCheck = "update.last_check"
)

// DefaultCheckInterval is the background cadence when the operator
// hasn't picked one.
const DefaultCheckInterval = "monthly"

// IntervalDuration maps a check_interval setting to a wall-clock
// duration. ok=false means "off" (no automatic checking). Unknown
// values fall back to the default rather than silently disabling.
func IntervalDuration(setting string) (time.Duration, bool) {
	switch setting {
	case "off":
		return 0, false
	case "daily":
		return 24 * time.Hour, true
	case "weekly":
		return 7 * 24 * time.Hour, true
	case "monthly":
		return 30 * 24 * time.Hour, true
	default:
		return 30 * 24 * time.Hour, true
	}
}

// ValidInterval reports whether s is a recognized check_interval value.
func ValidInterval(s string) bool {
	switch s {
	case "off", "daily", "weekly", "monthly":
		return true
	}
	return false
}

// LastCheck is the persisted result of the most recent release check.
type LastCheck struct {
	At            int64  `json:"at"` // unix ms
	LatestVersion string `json:"latest_version,omitempty"`
	URL           string `json:"url,omitempty"` // release page for humans
	Err           string `json:"err,omitempty"`
}

// Asset is one downloadable file attached to a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Release is the subset of the GitHub release object the updater needs.
type Release struct {
	TagName string  `json:"tag_name"`
	HTMLURL string  `json:"html_url"`
	Assets  []Asset `json:"assets"`
}

// Version is the release's semantic version without the tag's leading v.
func (r *Release) Version() string { return strings.TrimPrefix(r.TagName, "v") }

// BinaryAsset finds the prebuilt-binary tarball for a GOARCH
// (release naming: hoptrail_<ver>_linux_<arch>.tar.gz).
func (r *Release) BinaryAsset(goarch string) (Asset, bool) {
	suffix := fmt.Sprintf("_linux_%s.tar.gz", goarch)
	for _, a := range r.Assets {
		if strings.HasPrefix(a.Name, "hoptrail_") && strings.HasSuffix(a.Name, suffix) {
			return a, true
		}
	}
	return Asset{}, false
}

// ChecksumsAsset finds the sha256 manifest
// (release naming: hoptrail_<ver>_checksums.txt).
func (r *Release) ChecksumsAsset() (Asset, bool) {
	for _, a := range r.Assets {
		if strings.HasPrefix(a.Name, "hoptrail_") && strings.HasSuffix(a.Name, "_checksums.txt") {
			return a, true
		}
	}
	return Asset{}, false
}

// Newer reports whether the latest release version is strictly newer
// than the running build. Both sides tolerate the shapes git describe
// produces: "v0.5.0", "0.5.0", and dev builds like "v0.4.0-52-gabc"
// (compared by their base tag, so a build ahead of the latest release
// is NOT flagged as updatable). Unparseable versions ("dev") never
// claim an update — a dev build's operator knows what they're running.
func Newer(latest, running string) bool {
	lv, lok := parseSemver(latest)
	rv, rok := parseSemver(running)
	if !lok || !rok {
		return false
	}
	for i := range lv {
		if lv[i] != rv[i] {
			return lv[i] > rv[i]
		}
	}
	return false
}

func parseSemver(s string) ([3]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s = s[:i] // strip the -N-g<hash>[-dirty] dev suffix
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// Client fetches release metadata and assets. The zero value is not
// usable — use NewClient, or fill APIBase/Repo explicitly in tests.
type Client struct {
	// APIBase is the GitHub API root (tests point this at httptest).
	APIBase string
	// Repo is the owner/name to query.
	Repo string
	// HTTP is the client for both API and asset requests.
	HTTP *http.Client
}

// NewClient returns a production client against api.github.com.
func NewClient() *Client {
	return &Client{
		APIBase: "https://api.github.com",
		Repo:    DefaultRepo,
		HTTP:    &http.Client{Timeout: 5 * time.Minute},
	}
}

// Latest fetches the most recent published (non-draft, non-prerelease)
// release. A 404 means the repo has no releases yet.
func (c *Client) Latest(ctx context.Context) (*Release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", c.APIBase, c.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "hoptrail-updater")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("release check: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("release check: no releases published")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release check: GitHub responded %s", resp.Status)
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("release check: bad response: %w", err)
	}
	if rel.TagName == "" {
		return nil, errors.New("release check: response has no tag_name")
	}
	return &rel, nil
}

// DownloadBinary fetches the goarch-matched binary tarball from rel,
// verifies its sha256 against the release's checksums asset, extracts
// the hoptrail binary, and writes it to destPath (temp file + rename,
// so a failed download never sits at the destination). The caller owns
// destPath placement — the server stages it where the apply path looks.
func (c *Client) DownloadBinary(ctx context.Context, rel *Release, goarch, destPath string) error {
	binAsset, ok := rel.BinaryAsset(goarch)
	if !ok {
		return fmt.Errorf("release %s has no prebuilt binary for linux/%s — update by building from source", rel.TagName, goarch)
	}
	sumAsset, ok := rel.ChecksumsAsset()
	if !ok {
		return fmt.Errorf("release %s has no checksums asset — refusing unverified download", rel.TagName)
	}

	wantSum, err := c.fetchChecksum(ctx, sumAsset, binAsset.Name)
	if err != nil {
		return err
	}

	// Stream the tarball to a temp file next to the destination,
	// hashing as it lands.
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("staging dir: %w", err)
	}
	tarTmp, err := os.CreateTemp(filepath.Dir(destPath), ".download-*.tar.gz")
	if err != nil {
		return fmt.Errorf("staging temp: %w", err)
	}
	defer os.Remove(tarTmp.Name())

	body, err := c.get(ctx, binAsset.URL)
	if err != nil {
		tarTmp.Close()
		return fmt.Errorf("download %s: %w", binAsset.Name, err)
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tarTmp, hash), io.LimitReader(body, maxDownloadBytes+1))
	body.Close()
	if cerr := tarTmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", binAsset.Name, err)
	}
	if n > maxDownloadBytes {
		return fmt.Errorf("download %s exceeds %d bytes", binAsset.Name, int64(maxDownloadBytes))
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != wantSum {
		return fmt.Errorf("sha256 mismatch for %s: got %s, want %s — download corrupted or tampered", binAsset.Name, got, wantSum)
	}

	return extractBinary(tarTmp.Name(), destPath)
}

// fetchChecksum downloads the checksums manifest and returns the hex
// sha256 recorded for assetName.
func (c *Client) fetchChecksum(ctx context.Context, sumAsset Asset, assetName string) (string, error) {
	body, err := c.get(ctx, sumAsset.URL)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", sumAsset.Name, err)
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("download %s: %w", sumAsset.Name, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == assetName && len(fields[0]) == 64 {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("checksums asset has no entry for %s", assetName)
}

func (c *Client) get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "hoptrail-updater")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	return resp.Body, nil
}

// extractBinary pulls the hoptrail member out of a verified tar.gz and
// lands it at destPath via temp + rename, mode 0755.
func extractBinary(tarPath, destPath string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return errors.New("extract: tarball has no hoptrail binary")
		}
		if err != nil {
			return fmt.Errorf("extract: %w", err)
		}
		if filepath.Base(hdr.Name) != "hoptrail" || hdr.Typeflag != tar.TypeReg {
			continue
		}
		binTmp, err := os.CreateTemp(filepath.Dir(destPath), ".extract-*")
		if err != nil {
			return fmt.Errorf("extract temp: %w", err)
		}
		tmpName := binTmp.Name()
		defer os.Remove(tmpName) // no-op after successful rename
		n, err := io.Copy(binTmp, io.LimitReader(tr, maxDownloadBytes+1))
		if cerr := binTmp.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("extract: %w", err)
		}
		if n > maxDownloadBytes {
			return fmt.Errorf("extracted binary exceeds %d bytes", int64(maxDownloadBytes))
		}
		// Same cheap shape check as the upload path: catches a
		// wrong-platform or corrupted member before it reaches apply.
		head := make([]byte, 4)
		if hf, rerr := os.Open(tmpName); rerr == nil {
			_, _ = io.ReadFull(hf, head)
			hf.Close()
		}
		if string(head) != "\x7fELF" {
			return errors.New("extract: tarball member is not a Linux executable")
		}
		if err := os.Chmod(tmpName, 0o755); err != nil {
			return fmt.Errorf("extract chmod: %w", err)
		}
		if err := os.Rename(tmpName, destPath); err != nil {
			return fmt.Errorf("extract stage: %w", err)
		}
		return nil
	}
}
