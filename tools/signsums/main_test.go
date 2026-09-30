package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignedMessageFormat(t *testing.T) {
	// The literal format is shared with core.updateSignedMessage; both pin it.
	sums := "ab  mtssh-linux-amd64\ncd  mtssh-windows-amd64.exe\n"
	want := "mtssh-release 1.2.0-rc.1\n" + sums
	if got := string(signedMessage("1.2.0-rc.1", []byte(sums))); got != want {
		t.Fatalf("signedMessage = %q, want %q", got, want)
	}
}

func TestSignAndVerify(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	t.Setenv("UPDATE_SIGNING_KEY", base64.StdEncoding.EncodeToString(priv.Seed()))
	sums := filepath.Join(t.TempDir(), "SHA256SUMS")
	content := []byte(strings.Repeat("ab", 32) + "  mtssh-linux-amd64\n")
	if err := os.WriteFile(sums, content, 0644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	for _, v := range []string{"", "v1.2.0", "1.2.0\n", "1 2"} {
		if err := run([]string{"-version", v, sums}, &out); err == nil {
			t.Fatalf("signed with version %q", v)
		}
	}
	if err := run([]string{sums}, &out); err == nil {
		t.Fatal("signed without -version")
	}
	if err := run([]string{"-version", "1.2.0", sums}, &out); err != nil {
		t.Fatal(err)
	}

	// The signature is over the literal message, as the updater checks it.
	sig, err := os.ReadFile(sums + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || !ed25519.Verify(pub, append([]byte("mtssh-release 1.2.0\n"), content...), raw) {
		t.Fatalf("signature is not over the documented message: %q", sig)
	}

	if err := run([]string{"-verify", "-version", "1.2.0", "-pubkey", pubB64, sums}, &out); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := run([]string{"-verify", "-version", "1.2.1", "-pubkey", pubB64, sums}, &out); err == nil {
		t.Fatal("verified for another version")
	}
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := run([]string{"-verify", "-version", "1.2.0", "-pubkey", base64.StdEncoding.EncodeToString(otherPub), sums}, &out); err == nil {
		t.Fatal("verified with another key")
	}
	for _, key := range []string{"", "not base64", base64.StdEncoding.EncodeToString(pub[:16])} {
		if err := run([]string{"-verify", "-version", "1.2.0", "-pubkey", key, sums}, &out); err == nil {
			t.Fatalf("verified with public key %q", key)
		}
	}
	if err := os.WriteFile(sums, append(content, "cd  extra\n"...), 0644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-verify", "-version", "1.2.0", "-pubkey", pubB64, sums}, &out); err == nil {
		t.Fatal("verified modified checksums")
	}
}
