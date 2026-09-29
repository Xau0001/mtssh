// Command signsums signs a release's SHA256SUMS for the MTSSH updater.
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
//	UPDATE_SIGNING_KEY=… go run ./tools/signsums SHA256SUMS
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	genkey := flag.Bool("genkey", false, "print a new key pair")
	flag.Parse()

	if *genkey {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			fail(err)
		}
		fmt.Println("UPDATE_SIGNING_KEY (secret):", base64.StdEncoding.EncodeToString(priv.Seed()))
		fmt.Println("UPDATE_PUBLIC_KEY  (public):", base64.StdEncoding.EncodeToString(pub))
		return
	}
	if flag.NArg() != 1 {
		fail(fmt.Errorf("usage: signsums [-genkey] SHA256SUMS"))
	}

	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("UPDATE_SIGNING_KEY")))
	if err != nil || len(seed) != ed25519.SeedSize {
		fail(fmt.Errorf("UPDATE_SIGNING_KEY must be a base64 Ed25519 seed (see -genkey)"))
	}
	key := ed25519.NewKeyFromSeed(seed)

	path := flag.Arg(0)
	data, err := os.ReadFile(path)
	if err != nil {
		fail(err)
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(key, data))
	if err := os.WriteFile(path+".sig", []byte(sig+"\n"), 0644); err != nil {
		fail(err)
	}
	fmt.Println("signed", path, "with public key", base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "signsums:", err)
	os.Exit(1)
}
