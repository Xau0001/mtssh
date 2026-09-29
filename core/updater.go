package core

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const githubOwner = "Xau0001"
const githubRepo = "mtssh"

// checksumAsset is the release asset listing "<sha256>  <file name>" lines.
const checksumAsset = "SHA256SUMS"

type githubRelease struct {
	TagName string        `json:"tag_name"`
	HTMLURL string        `json:"html_url"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// Release describes the newest published release.
type Release struct {
	Version     string // without "v" prefix
	PageURL     string // release page, for manual download
	AssetName   string // binary for this OS/arch, empty if none
	BinaryURL   string // download URL of AssetName
	ChecksumURL string // download URL of SHA256SUMS, empty if not published
}

// SafePageURL returns the release page URL if it is an https link, else "".
func (r Release) SafePageURL() string {
	if !isHTTPS(r.PageURL) {
		return ""
	}
	return r.PageURL
}

func isHTTPS(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

// CanSelfUpdate reports whether SelfUpdate can install r in place.
// On Windows the running executable is locked, and releases without a
// checksum file cannot be verified — both fall back to the release page.
func (r Release) CanSelfUpdate() bool {
	return runtime.GOOS != "windows" && isHTTPS(r.BinaryURL) && isHTTPS(r.ChecksumURL)
}

// LatestRelease fetches the newest GitHub release.
func LatestRelease() (Release, error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", githubOwner, githubRepo)
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("User-Agent", "mtssh-updater")
	resp, err := client.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("GitHub API: HTTP %d", resp.StatusCode)
	}

	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return Release{}, err
	}

	r := Release{
		Version: strings.TrimPrefix(rel.TagName, "v"),
		PageURL: rel.HTMLURL,
	}
	want := platformAsset()
	for _, a := range rel.Assets {
		switch a.Name {
		case want:
			r.AssetName, r.BinaryURL = a.Name, a.BrowserDownloadURL
		case checksumAsset:
			r.ChecksumURL = a.BrowserDownloadURL
		}
	}
	return r, nil
}

// platformAsset returns the release asset name for this build, e.g.
// "mtssh-linux-amd64" or "mtssh-windows-amd64.exe". Platforms without a
// published binary simply find no match.
func platformAsset() string {
	name := fmt.Sprintf("mtssh-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// IsNewer reports whether latest is strictly newer than current (semver).
func IsNewer(current, latest string) bool {
	c := parseSemver(current)
	l := parseSemver(latest)
	for i := range l {
		if i >= len(c) {
			return true
		}
		if l[i] > c[i] {
			return true
		}
		if l[i] < c[i] {
			return false
		}
	}
	return false
}

func parseSemver(v string) []int {
	v = strings.TrimPrefix(v, "v")
	// Ignore pre-release / build metadata ("1.2.0-rc1", "1.2.0+abc").
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

// SelfUpdate downloads the release binary, verifies it against the
// release's SHA256SUMS and atomically replaces the running executable.
// progress is called with values in [0, 1] during the download.
func SelfUpdate(r Release, progress func(float64)) error {
	if !r.CanSelfUpdate() {
		return fmt.Errorf("self-update not supported for this release; download it from %s", r.PageURL)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("executable path: %w", err)
	}
	// Replace the real file, not a symlink pointing to it.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return installUpdate(&http.Client{Timeout: 10 * time.Minute}, r, exe, progress)
}

// installUpdate downloads r's binary, verifies it and replaces target.
func installUpdate(client *http.Client, r Release, target string, progress func(float64)) error {
	sums, err := download(client, r.ChecksumURL)
	if err != nil {
		return fmt.Errorf("checksums: %w", err)
	}
	defer sums.Body.Close()
	want, err := findChecksum(io.LimitReader(sums.Body, 1<<20), r.AssetName)
	if err != nil {
		return err
	}

	resp, err := download(client, r.BinaryURL)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	// Temp file next to the target so the final rename stays on one filesystem.
	f, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".update-*")
	if err != nil {
		return fmt.Errorf("temp file (missing permissions?): %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename

	h := sha256.New()
	pw := &progressWriter{total: resp.ContentLength, fn: progress}
	if _, err := io.Copy(io.MultiWriter(f, h, pw), resp.Body); err != nil {
		f.Close()
		return fmt.Errorf("download: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if got := h.Sum(nil); !bytes.Equal(got, want) {
		return fmt.Errorf("checksum mismatch for %s: got %x, want %x", r.AssetName, got, want)
	}
	if err := os.Chmod(tmp, 0755); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("failed to replace binary (missing permissions?): %w", err)
	}
	return nil
}

func download(client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mtssh-updater")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

// findChecksum parses sha256sum output and returns the digest for name.
func findChecksum(r io.Reader, name string) ([]byte, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		sum, err := hex.DecodeString(fields[0])
		if err != nil || len(sum) != sha256.Size {
			return nil, fmt.Errorf("malformed checksum for %s", name)
		}
		return sum, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("no checksum published for %s", name)
}

type progressWriter struct {
	done, total int64
	fn          func(float64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if p.fn != nil && p.total > 0 {
		p.fn(float64(p.done) / float64(p.total))
	}
	return len(b), nil
}
