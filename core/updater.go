package core

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const githubOwner = "Xau0001"
const githubRepo = "mtssh"

// checksumAsset is the release asset listing "<sha256>  <file name>" lines.
const checksumAsset = "SHA256SUMS"

// signatureAsset holds the base64 Ed25519 signature of the release version
// and checksumAsset (see updateSignedMessage), made with the key whose
// public half is UpdatePublicKey (see tools/signsums).
const signatureAsset = "SHA256SUMS.sig"

// updateVersionPattern is what a signed release version may consist of.
// The version is part of the signed message; a line break in it could make
// two different (version, SHA256SUMS) pairs sign the same bytes.
var updateVersionPattern = regexp.MustCompile(`^[0-9A-Za-z.+-]+$`)

// UpdatePublicKey is the base64 Ed25519 public key that release checksums
// must be signed with. It is set at build time
// (-ldflags "-X mtssh/core.UpdatePublicKey=…"); builds without it only
// point to the release page and announce every newer release unchecked.
// Builds with it announce only signed releases (see VerifyRelease), also
// where they don't update in place. The checksums alone come from the
// same place as the binary, so they don't prove who published it.
var UpdatePublicKey = ""

// Packaged is "true" in builds installed by a package manager or installer
// (-ldflags "-X mtssh/core.Packaged=true"). Those are updated there; the
// updater only points to the release page.
var Packaged = ""

// Size limits for what the updater reads from the network.
const (
	maxReleaseJSON = 5 << 20   // GitHub API response
	maxChecksums   = 1 << 20   // SHA256SUMS
	maxSignature   = 1 << 10   // SHA256SUMS.sig
	maxBinarySize  = 256 << 20 // downloaded executable (current builds: ~30 MB)
)

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
	Version      string // without "v" prefix
	PageURL      string // release page, for manual download
	AssetName    string // binary for this OS/arch, empty if none
	BinaryURL    string // download URL of AssetName
	ChecksumURL  string // download URL of SHA256SUMS, empty if not published
	SignatureURL string // download URL of SHA256SUMS.sig, empty if not published
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
// On Windows the running executable is locked; packaged builds are updated
// by their package manager; releases without signed checksums, and builds
// without the key to check them, cannot be verified. All of these fall
// back to the release page.
func (r Release) CanSelfUpdate() bool {
	return runtime.GOOS != "windows" && Packaged != "true" && updatePublicKey() != nil &&
		isHTTPS(r.BinaryURL) && isHTTPS(r.ChecksumURL) && isHTTPS(r.SignatureURL)
}

// CheckSelfUpdate reports why the running executable cannot be replaced
// in place, or nil if it can: MTSSH must not run as root (a GUI as root to
// get past permissions would overwrite files a package manager owns), and
// the executable's directory must be writable by the user.
func CheckSelfUpdate() error {
	if os.Geteuid() == 0 {
		return errors.New("MTSSH is running as root")
	}
	exe, err := executablePath()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(exe), filepath.Base(exe)+".update-*")
	if err != nil {
		return fmt.Errorf("%s is not writable", filepath.Dir(exe))
	}
	f.Close()
	os.Remove(f.Name())
	return nil
}

// executablePath returns the running executable, symlinks resolved.
func executablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("executable path: %w", err)
	}
	// Replace the real file, not a symlink pointing to it.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// updatePublicKey decodes UpdatePublicKey; nil if unset or malformed.
func updatePublicKey() ed25519.PublicKey {
	key, err := base64.StdEncoding.DecodeString(UpdatePublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(key)
}

// httpsOnly refuses redirects away from https (and redirect loops).
func httpsOnly(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to %s", req.URL.Redacted())
	}
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	return nil
}

// LatestRelease fetches the newest GitHub release.
func LatestRelease() (Release, error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", githubOwner, githubRepo)
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: httpsOnly}
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseJSON)).Decode(&rel); err != nil {
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
		case signatureAsset:
			r.SignatureURL = a.BrowserDownloadURL
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

// IsNewer reports whether latest is a newer version than current, following
// semantic versioning: missing parts count as 0 ("1.0" = "1.0.0"), and a
// pre-release is older than its release ("1.2.0-rc1" < "1.2.0").
func IsNewer(current, latest string) bool {
	return compareVersions(latest, current) > 0
}

// ShouldOffer reports whether latest is an update to offer to users of
// current: it must be newer, and a pre-release is offered only to users who
// already run one. GitHub's pre-release flag is not signed, so a token
// holder could make a signed release candidate the latest release; its
// version, which is signed, still says it is a pre-release.
func ShouldOffer(current, latest string) bool {
	_, currentPre := splitVersion(current)
	_, latestPre := splitVersion(latest)
	if latestPre != "" && currentPre == "" {
		return false
	}
	return IsNewer(current, latest)
}

func compareVersions(a, b string) int {
	an, apre := splitVersion(a)
	bn, bpre := splitVersion(b)
	for i := 0; i < len(an) || i < len(bn); i++ {
		var x, y int
		if i < len(an) {
			x = an[i]
		}
		if i < len(bn) {
			y = bn[i]
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	switch {
	case apre == bpre:
		return 0
	case apre == "":
		return 1
	case bpre == "":
		return -1
	}
	return comparePrerelease(apre, bpre)
}

// splitVersion splits "v1.2.3-rc.1+build" into [1 2 3] and "rc.1".
func splitVersion(v string) ([]int, string) {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	var pre string
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v, pre = v[:i], v[i+1:]
	}
	parts := strings.Split(v, ".")
	nums := make([]int, len(parts))
	for i, p := range parts {
		nums[i], _ = strconv.Atoi(p)
	}
	return nums, pre
}

// comparePrerelease orders pre-release identifiers as semver does:
// numeric ones numerically and before alphanumeric ones.
func comparePrerelease(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, xerr := strconv.Atoi(as[i])
		y, yerr := strconv.Atoi(bs[i])
		switch {
		case xerr == nil && yerr == nil:
			if x != y {
				if x > y {
					return 1
				}
				return -1
			}
		case xerr == nil:
			return -1
		case yerr == nil:
			return 1
		default:
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(as) > len(bs):
		return 1
	case len(as) < len(bs):
		return -1
	}
	return 0
}

// SelfUpdate downloads the release binary, verifies it against the
// release's SHA256SUMS, signed together with r.Version, and atomically
// replaces the running executable.
// progress is called with values in [0, 1] during the download.
func SelfUpdate(r Release, progress func(float64)) error {
	if !r.CanSelfUpdate() {
		return fmt.Errorf("self-update not supported for this release; download it from %s", r.PageURL)
	}
	if err := CheckSelfUpdate(); err != nil {
		return err
	}
	exe, err := executablePath()
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Minute, CheckRedirect: httpsOnly}
	return installUpdate(client, r, exe, progress)
}

// VerifyRelease checks, before r is announced, that its SHA256SUMS is
// signed with the release key for r.Version. Builds with the key announce
// nothing else, also those that only open the release page (Windows,
// packages): a stolen token could publish a release without
// SHA256SUMS.sig, and users would be sent to download the attacker's
// binary from the genuine release page. Builds without the key have
// nothing to check against and return nil.
func VerifyRelease(r Release) error {
	key := updatePublicKey()
	if key == nil {
		return nil
	}
	// Same limit as the release check: both files are small.
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: httpsOnly}
	return verifyRelease(client, key, r)
}

// verifyRelease is VerifyRelease with the key and HTTP client given.
func verifyRelease(client *http.Client, key ed25519.PublicKey, r Release) error {
	if !isHTTPS(r.ChecksumURL) || !isHTTPS(r.SignatureURL) {
		return fmt.Errorf("release %q publishes no signed checksums (%s and %s)", r.Version, checksumAsset, signatureAsset)
	}
	_, err := signedChecksums(client, key, r)
	return err
}

// signedChecksums downloads r's SHA256SUMS and SHA256SUMS.sig and returns
// the checksums if they are signed with key for r.Version.
func signedChecksums(client *http.Client, key ed25519.PublicKey, r Release) ([]byte, error) {
	sums, err := fetch(client, r.ChecksumURL, maxChecksums)
	if err != nil {
		return nil, fmt.Errorf("checksums: %w", err)
	}
	sig, err := fetch(client, r.SignatureURL, maxSignature)
	if err != nil {
		return nil, fmt.Errorf("checksum signature: %w", err)
	}
	if err := verifyChecksums(key, r.Version, sums, sig); err != nil {
		return nil, err
	}
	return sums, nil
}

// installUpdate downloads r's binary, verifies it against the checksums
// signed for r.Version and replaces target.
func installUpdate(client *http.Client, r Release, target string, progress func(float64)) error {
	// Checked again, not taken from VerifyRelease: the release assets can
	// change between the announcement and the user's answer.
	sums, err := signedChecksums(client, updatePublicKey(), r)
	if err != nil {
		return err
	}
	want, err := findChecksum(bytes.NewReader(sums), r.AssetName)
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
	n, err := io.Copy(io.MultiWriter(f, h, pw), io.LimitReader(resp.Body, maxBinarySize+1))
	if err != nil {
		f.Close()
		return fmt.Errorf("download: %w", err)
	}
	if n > maxBinarySize {
		f.Close()
		return fmt.Errorf("download: %s is larger than %d MB", r.AssetName, maxBinarySize>>20)
	}
	// On disk before the rename: a crash must not leave an empty binary.
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("write: %w", err)
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
	if d, err := os.Open(filepath.Dir(target)); err == nil {
		d.Sync() // persist the rename; not supported everywhere
		d.Close()
	}
	return nil
}

// updateSignedMessage returns the bytes the release key signs: a line
// naming the release version (the tag without "v"), then SHA256SUMS exactly
// as published. The checksums alone name the same files in every release,
// so a signature over them only would let an old, vulnerable release be
// republished under a newer tag. tools/signsums builds the same message.
func updateSignedMessage(version string, sums []byte) []byte {
	msg := make([]byte, 0, len("mtssh-release \n")+len(version)+len(sums))
	msg = append(msg, "mtssh-release "+version+"\n"...)
	return append(msg, sums...)
}

// verifyChecksums checks sig, the base64 signature of version and sums (see
// updateSignedMessage), against key. version is the release being
// installed, so a signature made for another release does not verify.
func verifyChecksums(key ed25519.PublicKey, version string, sums, sig []byte) error {
	if key == nil {
		return errors.New("this build has no key to verify updates")
	}
	if !updateVersionPattern.MatchString(version) {
		return fmt.Errorf("invalid release version %q", version)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || !ed25519.Verify(key, updateSignedMessage(version, sums), raw) {
		return fmt.Errorf("the checksums of release %s are not signed with the MTSSH release key", version)
	}
	return nil
}

// fetch downloads url, at most max bytes.
func fetch(client *http.Client, url string, max int64) ([]byte, error) {
	resp, err := download(client, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("larger than %d bytes", max)
	}
	return data, nil
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
