package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/preston-peterson/hoptrail/internal/release"
)

// fakeReleaseSource implements ReleaseSource without any network: the
// "download" writes a fake ELF blob whose marker the fixture's
// StagedVersion hook reads back.
type fakeReleaseSource struct {
	mu        sync.Mutex
	rel       *release.Release
	latestErr error
	dlErr     error
	dlCount   int
	dlArch    string
}

func (f *fakeReleaseSource) Latest(context.Context) (*release.Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.latestErr != nil {
		return nil, f.latestErr
	}
	return f.rel, nil
}

func (f *fakeReleaseSource) DownloadBinary(_ context.Context, rel *release.Release, goarch, destPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dlErr != nil {
		return f.dlErr
	}
	f.dlCount++
	f.dlArch = goarch
	if err := os.MkdirAll(strings.TrimSuffix(destPath, "/hoptrail"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(destPath, elfBlob(rel.Version()), 0o755)
}

func newReleaseFixture(t *testing.T) (*updateFixture, *fakeReleaseSource) {
	t.Helper()
	src := &fakeReleaseSource{
		rel: &release.Release{
			TagName: "v0.6.0",
			HTMLURL: "https://example.test/releases/v0.6.0",
			Assets: []release.Asset{
				{Name: "hoptrail_0.6.0_checksums.txt"},
				{Name: "hoptrail_0.6.0_linux_amd64.tar.gz"},
				{Name: "hoptrail_0.6.0_linux_arm64.tar.gz"},
			},
		},
	}
	uf := newUpdateFixture(t, func(c *Config) {
		c.Version = "v0.5.0" // parseable, older than the fake release
		c.ReleaseSource = src
	})
	return uf, src
}

func postJSON(t *testing.T, url string, body string) *http.Response {
	t.Helper()
	res, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return res
}

func TestUpdateStatus_ReleaseFieldsDefault(t *testing.T) {
	uf, _ := newReleaseFixture(t)
	st := uf.status(t)
	if !st.ReleaseAvailable {
		t.Error("release_available = false, want true with a source wired")
	}
	if st.CheckInterval != "monthly" {
		t.Errorf("check_interval = %q, want monthly default", st.CheckInterval)
	}
	if st.LastCheck != nil {
		t.Errorf("last_check = %+v, want nil before any check", st.LastCheck)
	}
	if st.Arch == "" {
		t.Error("arch missing from update status")
	}
}

func TestUpdateStatus_NoSource(t *testing.T) {
	uf := newUpdateFixture(t)
	st := uf.status(t)
	if st.ReleaseAvailable {
		t.Error("release_available = true without a source")
	}
	res := postJSON(t, uf.ts.URL+"/api/update/check", "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotImplemented {
		t.Errorf("check without source = %d, want 501", res.StatusCode)
	}
	res2 := postJSON(t, uf.ts.URL+"/api/update/download", "")
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusNotImplemented {
		t.Errorf("download without source = %d, want 501", res2.StatusCode)
	}
}

func TestUpdateCheck(t *testing.T) {
	uf, _ := newReleaseFixture(t)
	res := postJSON(t, uf.ts.URL+"/api/update/check", "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("check = %d", res.StatusCode)
	}
	var chk updateCheckJSON
	if err := json.NewDecoder(res.Body).Decode(&chk); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if chk.LatestVersion != "0.6.0" || !chk.UpdateAvailable || chk.CheckedAt == 0 {
		t.Errorf("check = %+v", chk)
	}
	if chk.ReleaseURL != "https://example.test/releases/v0.6.0" {
		t.Errorf("release_url = %q", chk.ReleaseURL)
	}

	// The result persists — a fresh GET sees it without re-checking.
	st := uf.status(t)
	if st.LastCheck == nil || st.LastCheck.LatestVersion != "0.6.0" || !st.LastCheck.UpdateAvailable {
		t.Errorf("status last_check = %+v", st.LastCheck)
	}
}

func TestUpdateCheck_UpToDate(t *testing.T) {
	uf, src := newReleaseFixture(t)
	src.mu.Lock()
	src.rel.TagName = "v0.5.0" // same as running
	src.mu.Unlock()
	res := postJSON(t, uf.ts.URL+"/api/update/check", "")
	defer res.Body.Close()
	var chk updateCheckJSON
	json.NewDecoder(res.Body).Decode(&chk)
	if chk.UpdateAvailable {
		t.Errorf("update_available = true for same version: %+v", chk)
	}
}

func TestUpdateCheck_FetchError(t *testing.T) {
	uf, src := newReleaseFixture(t)
	src.mu.Lock()
	src.latestErr = errors.New("github unreachable")
	src.mu.Unlock()
	res := postJSON(t, uf.ts.URL+"/api/update/check", "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("check = %d, want 200 with error in body", res.StatusCode)
	}
	var chk updateCheckJSON
	json.NewDecoder(res.Body).Decode(&chk)
	if chk.Error == "" || chk.UpdateAvailable {
		t.Errorf("check = %+v, want recorded error", chk)
	}
}

func TestUpdateDownloadThenApply(t *testing.T) {
	uf, src := newReleaseFixture(t)
	res := postJSON(t, uf.ts.URL+"/api/update/download", "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("download = %d", res.StatusCode)
	}
	var staged updateStagedJSON
	if err := json.NewDecoder(res.Body).Decode(&staged); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !staged.Present || staged.Version != "v-0.6.0" {
		t.Errorf("staged = %+v", staged)
	}
	src.mu.Lock()
	if src.dlCount != 1 || (src.dlArch != "amd64" && src.dlArch != "arm64") {
		t.Errorf("download calls = %d arch = %q", src.dlCount, src.dlArch)
	}
	src.mu.Unlock()

	// A download also records the check.
	st := uf.status(t)
	if st.LastCheck == nil || st.LastCheck.LatestVersion != "0.6.0" {
		t.Errorf("status last_check after download = %+v", st.LastCheck)
	}

	// The staged binary rides the EXISTING apply path untouched.
	resApply := postJSON(t, uf.ts.URL+"/api/update/apply", "")
	defer resApply.Body.Close()
	if resApply.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resApply.Body)
		t.Fatalf("apply = %d: %s", resApply.StatusCode, body)
	}
	live, err := os.ReadFile(uf.installDir + "/bin/hoptrail")
	if err != nil {
		t.Fatalf("read live: %v", err)
	}
	if !bytes.Equal(live, elfBlob("0.6.0")) {
		t.Error("apply did not install the downloaded binary")
	}
}

func TestUpdateDownload_Error(t *testing.T) {
	uf, src := newReleaseFixture(t)
	src.mu.Lock()
	src.dlErr = errors.New("sha256 mismatch")
	src.mu.Unlock()
	res := postJSON(t, uf.ts.URL+"/api/update/download", "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("download = %d, want 502", res.StatusCode)
	}
	if st := uf.status(t); st.Staged.Present {
		t.Error("failed download must not leave a staged binary")
	}
}

func TestUpdatePatch_CheckInterval(t *testing.T) {
	uf, _ := newReleaseFixture(t)
	req, _ := http.NewRequest(http.MethodPatch, uf.ts.URL+"/api/update", strings.NewReader(`{"check_interval":"weekly"}`))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d", res.StatusCode)
	}
	var st updateStatusResponse
	json.NewDecoder(res.Body).Decode(&st)
	if st.CheckInterval != "weekly" {
		t.Errorf("check_interval = %q after PATCH", st.CheckInterval)
	}

	bad, _ := http.NewRequest(http.MethodPatch, uf.ts.URL+"/api/update", strings.NewReader(`{"check_interval":"hourly"}`))
	resBad, err := http.DefaultClient.Do(bad)
	if err != nil {
		t.Fatalf("PATCH bad: %v", err)
	}
	defer resBad.Body.Close()
	if resBad.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid interval = %d, want 400", resBad.StatusCode)
	}
}

func TestStatus_SurfacesUpdateAvailable(t *testing.T) {
	uf, _ := newReleaseFixture(t)
	postJSON(t, uf.ts.URL+"/api/update/check", "").Body.Close()

	res, err := http.Get(uf.ts.URL + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	defer res.Body.Close()
	var st struct {
		Update struct {
			LatestVersion   string `json:"latest_version"`
			UpdateAvailable bool   `json:"update_available"`
			LastCheckAt     *int64 `json:"last_check_at"`
		} `json:"update"`
	}
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.Update.LatestVersion != "0.6.0" || !st.Update.UpdateAvailable || st.Update.LastCheckAt == nil {
		t.Errorf("status update = %+v", st.Update)
	}
}
