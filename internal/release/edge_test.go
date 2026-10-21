package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

// A pre-release or build suffix counts as its release on both sides, whatever dots it holds: 1.3.0-rc.1's ".1" is
// not a fourth part that would make it newer than 1.3.0.
func TestOlderEdges(t *testing.T) {
	for _, c := range []struct {
		v, than string
		older   bool
	}{
		{"1.3.0", "1.3.0-rc.1", false},
		{"1.3.0", "v1.3.0-rc.1", false},
		{"1.2.0", "1.2.0+build.7", false},
		{"1.3.0-rc.1", "1.3.0", false},
		{"1.3.0-rc.1", "1.3.0-rc.2", false},
		{"1.3.0-rc.2.5", "1.3.0", false},
		{"1.2.9-rc.1", "1.3.0", true},
		{"1.3.0", "1.2.9-rc.1.2.3", false},
		{"v1.2.3", "1.2.3", false},
		{"1.2.3", "v1.2.3", false},
		{"01.2.3", "1.2.3", false},
		{"1.2.3", "01.2.3", false},
		{"1.2", "1.2.0", true},
		{"1.2.0", "1.2", false},
		{"1.2.3", "1.2.3.1", true},
		{"1.10.0", "1.9.9", false},
		{"1.2.3", "1.2.99999999999999999999", true},
		{"dev", "1.0.0", true},
		{"", "1.0.0", true},
		{"-1.0.0", "1.0.0", true},
	} {
		if got := Older(c.v, c.than); got != c.older {
			t.Errorf("Older(%q, %q) = %v", c.v, c.than, got)
		}
	}
}

// Without a key everything passes, so the key can only be empty in builds that set none: a key that is set but
// unusable refuses everything instead.
func TestSignedWithABrokenKey(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte("release")))
	defer func() { Key = "" }()
	for _, k := range []string{" ", "not base64!", base64.StdEncoding.EncodeToString(pub[:31]), base64.StdEncoding.EncodeToString(append(pub, 0))} {
		Key = k
		if Signed([]byte("release"), sig) {
			t.Errorf("key %q accepted a signature", k)
		}
	}
	Key = base64.StdEncoding.EncodeToString(pub)
	if !Signed([]byte("release"), " "+sig+"\r\n") {
		t.Error("a signature with spaces around it was refused")
	}
	if Signed([]byte("release\n"), sig) || Signed([]byte("releas"), sig) {
		t.Error("a signature over other bytes was accepted")
	}
}

// Before orders murmur's own versions as Older does, with a pre-release before its release; Older keeps counting
// it as its release, as the coding tools' versions need.
func TestBefore(t *testing.T) {
	for _, c := range []struct {
		v, than string
		want    bool
	}{
		{"1.3.0-rc.1", "1.3.0", true}, {"1.3.0", "1.3.0-rc.1", false}, {"v1.3.0-rc.1", "1.3.0", true},
		{"1.3.0-rc.1+b7", "1.3.0", true}, {"1.3.0+b7", "1.3.0-rc.1", false}, {"1.3.0-rc.1", "1.3.0-rc.2", false},
		{"1.2.9", "1.3.0-rc.1", true}, {"1.3.1-rc.1", "1.3.0", false}, {"1.3.0", "1.3.0", false}, {"1.2.0", "1.10.0", true},
	} {
		if got := Before(c.v, c.than); got != c.want {
			t.Errorf("Before(%q, %q) = %v, want %v", c.v, c.than, got, c.want)
		}
	}
}
