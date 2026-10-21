// Package release is this build's version and how versions compare.
package release

import (
	"crypto/ed25519"
	"encoding/base64"
	"strconv"
	"strings"
)

// Version is set at build time (-ldflags "-X github.com/EledoneAi/murmur/v2/internal/release.Version=1.2.3").
var Version = "dev"

// Dev says whether this is a build from source without a version: it never updates itself.
func Dev() bool { return Version == "dev" }

// Older compares dotted versions such as 1.2.9 and 1.2.10, by the number each part starts with, ignoring a leading
// v and what follows a - or a +: 2.1.300-rc.1 counts as 2.1.300. A part without a number counts as older, so does a
// missing part.
func Older(v, than string) bool {
	num := func(part string) (int, error) {
		if i := strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
			part = part[:i]
		}
		return strconv.Atoi(part)
	}
	parts := func(v string) []string {
		v = strings.TrimPrefix(v, "v")
		if i := strings.IndexAny(v, "-+"); i >= 0 { // the .1 of -rc.1 is not a fourth part
			v = v[:i]
		}
		return strings.Split(v, ".")
	}
	a, b := parts(v), parts(than)
	for i := range b {
		if i >= len(a) {
			return true
		}
		x, err1 := num(a[i])
		y, err2 := num(b[i])
		if err1 != nil || err2 != nil {
			return true
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// Key is the base64 ed25519 public key releases are signed with, set at build time. Laptops install only binaries
// signed with it, so taking over a hub is not enough to run code on every laptop. Empty in builds from source that
// don't set one: then only the checksum is checked.
var Key = ""

// Signed says whether sig signs body with Key. Without a key, anything is.
func Signed(body []byte, sig string) bool {
	if Key == "" {
		return true
	}
	pub, err1 := base64.StdEncoding.DecodeString(Key)
	s, err2 := base64.StdEncoding.DecodeString(strings.TrimSpace(sig))
	return err1 == nil && err2 == nil && len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, body, s)
}

// Repo is where the project publishes its releases, for hubs that look for new ones.
var Repo = "EledoneAi/murmur"

// Tap is the Homebrew cask laptops install from, for the line the hub's front page shows.
var Tap = "eledoneai/tap/murmur"

// Before compares murmur's own versions: as Older, but a pre-release comes before its release, 1.3.0-rc.1 then
// 1.3.0, which Older counts as one, as the coding tools' versions need. Two pre-releases of one release count as one.
func Before(v, than string) bool {
	if Older(v, than) || Older(than, v) {
		return Older(v, than)
	}
	return pre(v) && !pre(than)
}

// pre says whether v is a pre-release, such as 1.3.0-rc.1. What follows a + is a build, hyphens included.
func pre(v string) bool {
	v, _, _ = strings.Cut(v, "+")
	return strings.Contains(v, "-")
}
