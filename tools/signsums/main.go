// Command signsums signs a release's SHA256SUMS for the MTSSH updater.
//
// The signature covers the release version together with the checksums:
//
//	"mtssh-release " + version + "\n" + <the bytes of SHA256SUMS>
//
// where version is the tag without the leading "v" (tag v1.2.0: 1.2.0).
// The updater checks it against the version of the release it installs,
// so the signed files of an old release can't be republished as a newer
// one. SHA256SUMS itself stays plain sha256sum output.
//
// Create a key pair once:
//
//	go run ./tools/signsums -genkey
//
// Keep the private key secret (e.g. as the UPDATE_SIGNING_KEY secret of a
// protected "release" environment) and set the public key as the
// UPDATE_PUBLIC_KEY repository variable; release builds embed it.
//
// Sign (writes SHA256SUMS.sig next to the file):
//
//	UPDATE_SIGNING_KEY=… go run ./tools/signsums -version 1.2.0 SHA256SUMS
//
// Check SHA256SUMS.sig against a public key (exit status 1 if it does not
// verify):
//
//	go run ./tools/signsums -verify -version 1.2.0 -pubkey … SHA256SUMS
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// versionPattern must match core.updateVersionPattern: the updater refuses
// any other version.
var versionPattern = regexp.MustCompile(`^[0-9A-Za-z.+-]+$`)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "signsums:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("signsums", flag.ContinueOnError)
	genkey := flags.Bool("genkey", false, "print a new key pair")
	verifyOnly := flags.Bool("verify", false, "check FILE.sig against -pubkey instead of signing")
	version := flags.String("version", "", "release version: the tag without the leading \"v\"")
	pubkey := flags.String("pubkey", "", "base64 Ed25519 public key (with -verify)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if *genkey {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, "UPDATE_SIGNING_KEY (secret):", base64.StdEncoding.EncodeToString(priv.Seed()))
		fmt.Fprintln(stdout, "UPDATE_PUBLIC_KEY  (public):", base64.StdEncoding.EncodeToString(pub))
		return nil
	}
	if flags.NArg() != 1 {
		return errors.New("usage: signsums -genkey | -version V SHA256SUMS | -verify -version V -pubkey KEY SHA256SUMS")
	}
	if err := checkVersion(*version); err != nil {
		return err
	}
	path := flags.Arg(0)
	sums, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	if *verifyOnly {
		sig, err := os.ReadFile(path + ".sig")
		if err != nil {
			return err
		}
		if err := verify(*pubkey, *version, sums, sig); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s.sig: good signature for version %s\n", path, *version)
		return nil
	}

	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("UPDATE_SIGNING_KEY")))
	if err != nil || len(seed) != ed25519.SeedSize {
		return errors.New("UPDATE_SIGNING_KEY must be a base64 Ed25519 seed (see -genkey)")
	}
	key := ed25519.NewKeyFromSeed(seed)
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(key, signedMessage(*version, sums)))
	if err := os.WriteFile(path+".sig", []byte(sig+"\n"), 0644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "signed %s for version %s with public key %s\n",
		path, *version, base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)))
	return nil
}

// signedMessage returns the bytes the release key signs. It must stay
// identical to core.updateSignedMessage; tests on both sides pin the
// format (this tool is stdlib-only, so it does not import core).
func signedMessage(version string, sums []byte) []byte {
	msg := make([]byte, 0, len("mtssh-release \n")+len(version)+len(sums))
	msg = append(msg, "mtssh-release "+version+"\n"...)
	return append(msg, sums...)
}

func checkVersion(version string) error {
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("-version %q: need the release version, e.g. 1.2.0 (letters, digits, \".\", \"+\", \"-\")", version)
	}
	// The updater compares with the tag minus "v"; v1.2.0 would never verify.
	if strings.HasPrefix(version, "v") {
		return fmt.Errorf("-version %q: leave out the leading \"v\" of the tag", version)
	}
	return nil
}

// verify checks sig, a base64 signature as written by signing, against the
// base64 public key pubkey, the way the updater does.
func verify(pubkey, version string, sums, sig []byte) error {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(pubkey))
	if err != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("-pubkey must be a base64 Ed25519 public key (see -genkey)")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || !ed25519.Verify(ed25519.PublicKey(key), signedMessage(version, sums), raw) {
		return fmt.Errorf("the signature does not verify for version %s with this public key", version)
	}
	return nil
}
