package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		{"1.2.0-rc1", "1.2.0", false},
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

func TestInstallUpdate(t *testing.T) {
	newBinary := []byte("#!/bin/sh\necho new\n")
	sum := sha256.Sum256(newBinary)
	sums := hex.EncodeToString(sum[:]) + "  mtssh-linux-amd64\n"

	mux := http.NewServeMux()
	mux.HandleFunc("/bin", func(w http.ResponseWriter, _ *http.Request) { w.Write(newBinary) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, sums) })
	mux.HandleFunc("/tampered", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("evil")) })
	srv := httptest.NewServer(mux) // unknown paths answer 404
	defer srv.Close()

	// httptest serves plain http, so call installUpdate directly (SelfUpdate
	// would refuse the non-https URLs).
	rel := func(bin string) Release {
		return Release{AssetName: "mtssh-linux-amd64", BinaryURL: srv.URL + bin, ChecksumURL: srv.URL + "/sums"}
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
