package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func TestOlder(t *testing.T) {
	for _, c := range []struct {
		v, than string
		older   bool
	}{
		{"1.2.9", "1.2.10", true}, {"v1.2.10", "1.2.9", false}, {"1.3.0", "1.3.0", false}, {"1.3.0-rc.1", "1.3.0", false},
		{"1.2", "1.2.1", true}, {"", "1.0.0", true}, {"dev", "1.0.0", true}, {"2.0.0", "1.9.9", false},
	} {
		if got := Older(c.v, c.than); got != c.older {
			t.Errorf("Older(%q, %q) = %v", c.v, c.than, got)
		}
	}
}

func TestSigned(t *testing.T) {
	if !Signed([]byte("anything"), "") {
		t.Fatal("without a key, checksums are all there is")
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	Key = base64.StdEncoding.EncodeToString(pub)
	defer func() { Key = "" }()
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte("release")))
	if !Signed([]byte("release"), sig) {
		t.Fatal("a good signature was refused")
	}
	if Signed([]byte("tampered"), sig) || Signed([]byte("release"), "") || Signed([]byte("release"), "bm90IGEgc2ln") {
		t.Fatal("a bad signature was accepted")
	}
}
