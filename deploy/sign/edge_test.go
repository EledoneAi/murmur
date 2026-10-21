package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignFileEdges(t *testing.T) {
	pub, priv := keygen()
	key, _ := base64.StdEncoding.DecodeString(pub)
	dir := t.TempDir()
	sums := filepath.Join(dir, "checksums.txt")
	_ = os.WriteFile(sums, []byte("00  murmur-linux-amd64\n"), 0o644)

	// no key, or not the private one from -keygen, signs nothing
	for _, k := range []string{"", " ", priv[:len(priv)-4], base64.StdEncoding.EncodeToString(make([]byte, 64))} {
		if err := signFile(sums, k); err == nil {
			t.Errorf("signed with %q", k)
		}
	}
	if _, err := os.Stat(sums + ".sig"); err == nil {
		t.Fatal("a bad key left a signature")
	}
	// a missing file is an error, not an empty signature
	if err := signFile(filepath.Join(dir, "missing.txt"), priv); err == nil {
		t.Error("signed a file that isn't there")
	}
	// a secret store's trailing newline is fine
	if err := signFile(sums, priv+"\n"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(sums + ".sig")
	sig, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if !ed25519.Verify(key, []byte("00  murmur-linux-amd64\n"), sig) {
		t.Fatal("the signature doesn't check out")
	}
	if st, _ := os.Stat(sums + ".sig"); st.Mode().Perm()&0o022 != 0 {
		t.Errorf("the signature is writable by others: %v", st.Mode())
	}
	// an empty file signs as empty: laptops then find no binary in it
	empty := filepath.Join(dir, "empty.txt")
	_ = os.WriteFile(empty, nil, 0o644)
	if err := signFile(empty, priv); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(empty + ".sig")
	sig, _ = base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if !ed25519.Verify(key, nil, sig) {
		t.Fatal("the empty file's signature doesn't check out")
	}
}
