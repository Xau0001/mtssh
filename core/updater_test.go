package core

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestIsNewer(t *testing.T) {
	tests := []struct {
		current, latest string
		want            bool
	}{
		{"1.0.0", "1.0.1", true},
		{"1.0.0", "1.1.0", true},
		{"1.9.0", "1.10.0", true},
		{"1.0", "1.0.1", true},
		{"1.0.1", "1.0.0", false},
		{"1.0.0", "1.0.0", false},
		{"v1.2.0", "1.2.0", false},
		{"1.2.0-rc1", "1.2.0", true},
		{"1.2.0", "1.2.0-rc1", false},
		{"1.2.0-rc1", "1.2.0-rc2", true},
		{"1.2.0-rc.2", "1.2.0-rc.10", true},
		{"1.2.0-rc.1", "1.2.0-rc.1.1", true},
		{"1.2.0-1", "1.2.0-alpha", true},
		{"1.0", "1.0.0", false},
		{"1.0.0", "1.0", false},
		{"1.2.0+build5", "1.2.0", false},
		{"1.2.0", "1.3.0-beta", true},
	}
	for _, tt := range tests {
		if got := IsNewer(tt.current, tt.latest); got != tt.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", tt.current, tt.latest, got, tt.want)
		}
	}
}

func TestFindChecksum(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	sums := sum + "  mtssh-linux-amd64\n" +
		strings.Repeat("cd", 32) + " *mtssh-windows-amd64.exe\n"

	got, err := findChecksum(strings.NewReader(sums), "mtssh-linux-amd64")
	if err != nil || hex.EncodeToString(got) != sum {
		t.Fatalf("linux: got %x, %v", got, err)
	}
	if _, err := findChecksum(strings.NewReader(sums), "mtssh-windows-amd64.exe"); err != nil {
		t.Fatalf("binary-mode entry: %v", err)
	}
	if _, err := findChecksum(strings.NewReader(sums), "mtssh-linux"); err == nil {
		t.Fatal("partial name must not match")
	}
	if _, err := findChecksum(strings.NewReader("zz  mtssh-linux-amd64\n"), "mtssh-linux-amd64"); err == nil {
		t.Fatal("malformed checksum accepted")
	}
}

// signingKey sets UpdatePublicKey to a new key for the test and returns
// its private half.
func signingKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	old := UpdatePublicKey
	UpdatePublicKey = base64.StdEncoding.EncodeToString(pub)
	t.Cleanup(func() { UpdatePublicKey = old })
	return priv
}

// updateSignRelease returns the base64 signature of version and sums in the
// format the release workflow publishes.
func updateSignRelease(priv ed25519.PrivateKey, version, sums string) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, updateSignedMessage(version, []byte(sums)))) + "\n"
}

func TestUpdateSignedMessageFormat(t *testing.T) {
	// The literal format is shared with tools/signsums; both pin it.
	sums := "ab  mtssh-linux-amd64\ncd  mtssh-windows-amd64.exe\n"
	want := "mtssh-release 1.2.0-rc.1\n" + sums
	if got := string(updateSignedMessage("1.2.0-rc.1", []byte(sums))); got != want {
		t.Fatalf("updateSignedMessage = %q, want %q", got, want)
	}
}

func TestUpdateVerifyChecksums(t *testing.T) {
	priv := signingKey(t)
	key := updatePublicKey()
	sums := strings.Repeat("ab", 32) + "  mtssh-linux-amd64\n"
	sig := []byte(updateSignRelease(priv, "1.2.0", sums))

	if err := verifyChecksums(key, "1.2.0", []byte(sums), sig); err != nil {
		t.Fatalf("valid signature: %v", err)
	}
	// Replay: the signed checksums of 1.2.0 published as another release.
	if err := verifyChecksums(key, "9.9.9", []byte(sums), sig); err == nil {
		t.Fatal("signature for 1.2.0 accepted for 9.9.9")
	}
	// The old format signed SHA256SUMS alone.
	oldSig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(sums)))
	if err := verifyChecksums(key, "1.2.0", []byte(sums), []byte(oldSig)); err == nil {
		t.Fatal("signature over SHA256SUMS alone accepted")
	}
	if err := verifyChecksums(key, "1.2.0", []byte(sums+"cd  extra\n"), sig); err == nil {
		t.Fatal("modified checksums accepted")
	}
	// An empty version, or one that could shift the message boundary, is
	// refused even when signed.
	for _, v := range []string{"", "1.2.0\nab", "1.2.0 ", "../1"} {
		if err := verifyChecksums(key, v, []byte(sums), []byte(updateSignRelease(priv, v, sums))); err == nil {
			t.Fatalf("version %q accepted", v)
		}
	}
	if err := verifyChecksums(nil, "1.2.0", []byte(sums), sig); err == nil {
		t.Fatal("accepted without a key")
	}
}

func TestInstallUpdate(t *testing.T) {
	priv := signingKey(t)
	newBinary := []byte("#!/bin/sh\necho new\n")
	sum := sha256.Sum256(newBinary)
	sums := hex.EncodeToString(sum[:]) + "  mtssh-linux-amd64\n"
	sig := updateSignRelease(priv, "1.2.0", sums)
	// Checksums for the tampered binary, signed with another key.
	evilSum := sha256.Sum256([]byte("evil"))
	evilSums := hex.EncodeToString(evilSum[:]) + "  mtssh-linux-amd64\n"
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	evilSig := updateSignRelease(otherKey, "1.2.0", evilSums)
	// The old format: SHA256SUMS signed without a version.
	oldSig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(sums)))

	mux := http.NewServeMux()
	mux.HandleFunc("/bin", func(w http.ResponseWriter, _ *http.Request) { w.Write(newBinary) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, sums) })
	mux.HandleFunc("/sig", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, sig) })
	mux.HandleFunc("/oldsig", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, oldSig) })
	mux.HandleFunc("/evilsums", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, evilSums) })
	mux.HandleFunc("/evilsig", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, evilSig) })
	mux.HandleFunc("/tampered", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("evil")) })
	srv := httptest.NewServer(mux) // unknown paths answer 404
	defer srv.Close()

	// httptest serves plain http, so call installUpdate directly (SelfUpdate
	// would refuse the non-https URLs).
	rel := func(bin string) Release {
		return Release{Version: "1.2.0", AssetName: "mtssh-linux-amd64", BinaryURL: srv.URL + bin,
			ChecksumURL: srv.URL + "/sums", SignatureURL: srv.URL + "/sig"}
	}
	target := filepath.Join(t.TempDir(), "mtssh")
	old := []byte("old binary")
	if err := os.WriteFile(target, old, 0755); err != nil {
		t.Fatal(err)
	}
	unchanged := func(name string) {
		t.Helper()
		if got, _ := os.ReadFile(target); !bytes.Equal(got, old) {
			t.Fatalf("%s: binary was replaced", name)
		}
		if leftovers, _ := filepath.Glob(target + ".update-*"); len(leftovers) > 0 {
			t.Fatalf("%s: temp files left behind: %v", name, leftovers)
		}
	}

	if err := installUpdate(srv.Client(), rel("/missing"), target, nil); err == nil {
		t.Fatal("HTTP 404 must fail")
	}
	unchanged("404")

	if err := installUpdate(srv.Client(), rel("/tampered"), target, nil); err == nil ||
		!strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered binary: err = %v", err)
	}
	unchanged("tampered")

	// Checksums that match the tampered binary but carry a foreign or no
	// signature are refused.
	for _, sigPath := range []string{"/evilsig", "/sig", "/missing"} {
		r := rel("/tampered")
		r.ChecksumURL, r.SignatureURL = srv.URL+"/evilsums", srv.URL+sigPath
		if err := installUpdate(srv.Client(), r, target, nil); err == nil {
			t.Fatalf("unsigned checksums (%s) accepted", sigPath)
		}
		unchanged("signature " + sigPath)
	}

	// Genuine assets of release 1.2.0, published under another version
	// (replay), without a version, or signed in the old format.
	replayed, noVersion, oldFormat := rel("/bin"), rel("/bin"), rel("/bin")
	replayed.Version = "9.9.9"
	noVersion.Version = ""
	oldFormat.SignatureURL = srv.URL + "/oldsig"
	for name, r := range map[string]Release{"replayed as 9.9.9": replayed, "no version": noVersion, "old format": oldFormat} {
		if err := installUpdate(srv.Client(), r, target, nil); err == nil {
			t.Fatalf("%s: accepted", name)
		}
		unchanged(name)
	}

	var last float64
	if err := installUpdate(srv.Client(), rel("/bin"), target, func(p float64) { last = p }); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, newBinary) {
		t.Fatalf("target = %q, want new binary", got)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0755 {
		t.Fatalf("mode = %v, want 0755", fi.Mode().Perm())
	}
	if last != 1 {
		t.Fatalf("final progress = %v, want 1", last)
	}
}

func TestSafePageURL(t *testing.T) {
	tests := map[string]string{
		"https://github.com/Xau0001/mtssh/releases/tag/v1.2.0": "https://github.com/Xau0001/mtssh/releases/tag/v1.2.0",
		"http://github.com/x": "",
		"file:///etc/passwd":  "",
		"javascript:alert(1)": "",
		"https:///no-host":    "",
		"":                    "",
	}
	for in, want := range tests {
		if got := (Release{PageURL: in}).SafePageURL(); got != want {
			t.Errorf("SafePageURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanSelfUpdate(t *testing.T) {
	r := Release{BinaryURL: "https://x/bin", ChecksumURL: "https://x/sums", SignatureURL: "https://x/sig"}
	old := UpdatePublicKey
	defer func() { UpdatePublicKey = old }()
	UpdatePublicKey = ""
	if r.CanSelfUpdate() {
		t.Fatal("self-update without a public key")
	}
	signingKey(t)
	if want := runtime.GOOS != "windows"; r.CanSelfUpdate() != want {
		t.Fatalf("CanSelfUpdate = %v, want %v", !want, want)
	}
	r.SignatureURL = ""
	if r.CanSelfUpdate() {
		t.Fatal("self-update without a signature")
	}
}
