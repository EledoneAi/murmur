package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestSignFile(t *testing.T) {
	pub, priv := keygen()
	bin := filepath.Join(t.TempDir(), "murmur-linux-amd64")
	_ = os.WriteFile(bin, []byte("a release"), 0o755)
	if err := signFile(bin, "not a key"); err == nil {
		t.Fatal("signed with a bad key")
	}
	if err := signFile(bin, priv); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(bin + ".sig")
	sig, _ := base64.StdEncoding.DecodeString(string(b[:len(b)-1]))
	key, _ := base64.StdEncoding.DecodeString(pub)
	if !ed25519.Verify(key, []byte("a release"), sig) || ed25519.Verify(key, []byte("another one"), sig) {
		t.Fatal("the signature doesn't check out")
	}
}
