// Command sign creates a release key and signs a release's checksums.txt with it. Laptops check checksums.txt.sig
// against the public keys built into them, then check each download against checksums.txt.
//
//	go run ./deploy/sign -keygen
//	    prints a new key pair. Build the public key into every binary (MURMUR_RELEASE_PUBKEY for make and
//	    GoReleaser, which pass it as -X github.com/EledoneAi/murmur/v2/internal/release.Key), and keep the
//	    private key only in your CI's secret store, for the release to read as MURMUR_RELEASE_KEY
//	MURMUR_RELEASE_KEY=<private key> go run ./deploy/sign dist/checksums.txt
//	    writes checksums.txt.sig next to it (GoReleaser runs this; see .goreleaser.yaml)
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-keygen" {
		pub, priv := keygen()
		fmt.Println("public key, to build into laptops:", pub)
		fmt.Println("private key, for MURMUR_RELEASE_KEY in your CI's secrets only:", priv)
		return
	}
	for _, f := range os.Args[1:] {
		if err := signFile(f, os.Getenv("MURMUR_RELEASE_KEY")); err != nil {
			fmt.Fprintln(os.Stderr, "sign:", err)
			os.Exit(1)
		}
	}
}

func keygen() (pub, priv string) {
	p, k, _ := ed25519.GenerateKey(rand.Reader)
	return base64.StdEncoding.EncodeToString(p), base64.StdEncoding.EncodeToString(k.Seed())
}

func signFile(path, key string) error {
	seed, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(seed) != ed25519.SeedSize {
		return errors.New("MURMUR_RELEASE_KEY must hold the private key from -keygen")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sig := ed25519.Sign(ed25519.NewKeyFromSeed(seed), b)
	return os.WriteFile(path+".sig", []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644)
}
