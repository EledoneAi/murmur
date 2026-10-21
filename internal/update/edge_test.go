package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/release"
)

// sumLine is one checksums.txt line for bin under name.
func sumLine(bin []byte, name string) string {
	sum := sha256.Sum256(bin)
	return hex.EncodeToString(sum[:]) + "  " + name
}

// A checksums.txt line only counts in the exact form "<hex>  <name>" for this laptop's binary: anything close to it
// is no entry at all, so a download is refused rather than checked against the wrong line.
func TestChecksumLinesThatAreNotThisBinary(t *testing.T) {
	bin := []byte("the new murmur")
	sum := sha256.Sum256(bin)
	good := hex.EncodeToString(sum[:])
	name := binaryName()
	for _, c := range []struct {
		why, sums string
		ok        bool
	}{
		{"exact", good + "  " + name + "\n", true},
		{"windows line ends", good + "  " + name + "\r\n", true},
		{"no line end", good + "  " + name, true},
		{"the first of two lines wins", good + "  " + name + "\n00  " + name + "\n", true},
		{"a wrong first line wins too", "00  " + name + "\n" + good + "  " + name + "\n", false},
		{"a path before the name", good + "  ./" + name + "\n", false},
		{"a parent folder", good + "  ../" + name + "\n", false},
		{"binary mode", good + " *" + name + "\n", false},
		{"three spaces", good + "   " + name + "\n", false},
		{"a tab", good + "\t" + name + "\n", false},
		{"a trailing space", good + "  " + name + " \n", false},
		{"upper case hex", strings.ToUpper(good) + "  " + name + "\n", false},
		{"a longer name", good + "  " + name + ".sbom\n", false},
		{"a shorter name", good + "  " + strings.TrimSuffix(name, runtime.GOARCH) + "\n", false},
		{"no sum", "  " + name + "\n", false},
		{"empty", "", false},
	} {
		_, err := Fetch(serve(t, []byte(c.sums), nil, bin))
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", c.why, err)
		}
	}
}

// With the project's key built in, a hub can't get a binary installed without a signature over the very bytes of
// its checksums.txt, and nothing it serves turns the check off.
func TestSignatureEdges(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	bin := []byte("the new murmur")
	sums := checksums(bin)
	sign := func(k ed25519.PrivateKey, b []byte) []byte {
		return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(k, b)))
	}
	release.Key = base64.StdEncoding.EncodeToString(pub)
	defer func() { release.Key = "" }()
	for _, c := range []struct {
		why       string
		sums, sig []byte
	}{
		{"no signature", sums, nil},
		{"an empty signature", sums, []byte("\n")},
		{"another key's signature", sums, sign(other, sums)},
		{"a signature over other checksums", append([]byte("00  murmur-plan9-mips\n"), sums...), sign(priv, sums)},
		{"a signature over the checksums less a line", sums, sign(priv, []byte(sumLine(bin, binaryName())+"\n"))},
		{"a signature over the binary", sums, sign(priv, bin)},
		{"the public key as a signature", sums, []byte(release.Key)},
	} {
		if _, err := Fetch(serve(t, c.sums, c.sig, bin)); err == nil {
			t.Errorf("%s: accepted", c.why)
		}
	}
	// a hub without the signature file is refused, not taken for a build without a key
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dl/checksums.txt", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(sums) })
	mux.HandleFunc("GET /dl/"+runtime.GOOS+"/"+runtime.GOARCH, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(bin) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if _, err := Fetch(srv.URL); err == nil {
		t.Error("a hub without checksums.txt.sig: accepted")
	}
	// a key that doesn't decode checks nothing in: every update is refused
	release.Key = "not base64!"
	if _, err := Fetch(serve(t, sums, sign(priv, sums), bin)); err == nil {
		t.Error("a broken key accepted a release")
	}
}

// A download larger than any binary is cut short, and then fails its checksum instead of filling the laptop's memory.
func TestDownloadTooLarge(t *testing.T) {
	big := make([]byte, 128<<20+1)
	if _, err := Fetch(serve(t, checksums(big), nil, big)); err == nil {
		t.Fatal("a download over 128 MiB was accepted")
	}
}

// A redirect is followed, but what it leads to is checked like the hub's own answer: the signature decides.
func TestRedirectsAreCheckedToo(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	release.Key = base64.StdEncoding.EncodeToString(pub)
	defer func() { release.Key = "" }()
	bin := []byte("the new murmur")
	sums := checksums(bin)
	other := serve(t, sums, []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sums))), []byte("not the binary"))
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other+r.URL.Path, http.StatusFound)
	}))
	defer hub.Close()
	if _, err := Fetch(hub.URL); err == nil {
		t.Fatal("a redirect to another binary was accepted")
	}
}

// A hub that isn't on a release, or says something that isn't a version, never counts as newer. A pre-release
// moves on to its release, and a release never back to its pre-release.
func TestNewerEdges(t *testing.T) {
	defer func(v string) { release.Version = v }(release.Version)
	for _, c := range []struct {
		this, hub string
		newer     bool
	}{
		{"1.2.0", "dev", false},
		{"1.2.0", "unknown", false},
		{"1.2.0", "latest", false},
		{"1.2.0", "v", false},
		{"1.2.0", "1.3", false},
		{"1.2.0", " 1.3.0", false},
		{"1.2.0", "1.3.0\x1b[2J", false},
		{"1.2.0", "v1.3.0", true},
		{"v1.2.0", "1.3.0", true},
		{"1.2.0", "01.2.0", false},
		{"1.2.0", "1.2.0+build.7", false},
		{"1.2.0", "1.2.1-rc.1", true},
		{"1.3.0-rc.1", "1.3.0", true},
		{"1.3.0-rc.1+build", "1.3.0", true},
		{"1.3.0-rc.1", "1.3.0+build", true},
		{"1.3.0", "1.3.0-rc.1", false},
		{"1.3.0-rc.1", "1.3.0-rc.1", false},
		{"1.3.0-rc.1", "1.2.9", false},
		{"1.3.0+build-7", "1.3.0", false},
		{"1.2.0", "1.2.99999999999999999999", true}, // a hub can always say so: the binary it serves is asked
	} {
		release.Version = c.this
		if got := Newer(c.hub); got != c.newer {
			t.Errorf("on %s, is %q newer? %v", c.this, c.hub, got)
		}
	}
}

// A genuine pre-release of this version is an older one: a hub that serves it doesn't take a laptop back to it.
func TestDownloadNeverGoesBackToAPreRelease(t *testing.T) {
	defer func(v string) { release.Version = v }(release.Version)
	release.Version = "1.3.0"
	exe := filepath.Join(t.TempDir(), "murmur")
	for _, prints := range []string{"murmur 1.3.0-rc.1", "murmur v1.3.0-rc.1", "murmur 1.3.0+build.7", "murmur 1.3.0.1-rc.1"} {
		bin := []byte("#!/bin/sh\necho '" + prints + "'\n")
		if v, err := download(serve(t, checksums(bin), nil, bin), exe); err == nil {
			t.Errorf("a binary printing %q was kept, as %s", prints, v)
		}
	}
}

// Everything Managed recognises, as the docs list it, and what it must not.
func TestManagedPrefixes(t *testing.T) {
	for exe, want := range map[string]string{
		"/opt/homebrew/Caskroom/murmur/2.1.0/murmur":                    "Homebrew",
		"/opt/homebrew/bin/murmur":                                      "Homebrew",
		"/usr/local/Cellar/murmur/2.1.0/bin/murmur":                     "Homebrew",
		"/usr/local/Caskroom/murmur/2.1.0/murmur":                       "Homebrew",
		"/home/linuxbrew/.linuxbrew/Cellar/murmur/2.1.0/bin/murmur":     "Homebrew",
		"/nix/store/0c3qyjqdg7yxkh0ivs8l6k8w7hbmbpmg-murmur/bin/murmur": "Nix",
		"/usr/local/bin/murmur":                                         "",
		"/opt/homebrewish/murmur":                                       "",
		"/home/someone/.local/bin/murmur":                               "",
		"/nix/storefront/murmur":                                        "",
	} {
		if got := Managed(exe); got != want {
			t.Errorf("%s: %q, want %q", exe, got, want)
		}
	}
	// a link into a package manager's folder is that package manager's
	if _, err := os.Stat("/opt/homebrew/bin"); err != nil {
		t.Skip("no Homebrew here to link into")
	}
	link := filepath.Join(t.TempDir(), "murmur")
	if err := os.Symlink("/opt/homebrew/bin", link); err != nil {
		t.Fatal(err)
	}
	if got := Managed(link); got != "Homebrew" {
		t.Errorf("a link into /opt/homebrew: %q", got)
	}
}

func TestInstallEdges(t *testing.T) {
	// an earlier update's murmur.prev gives way to the binary this one replaces
	exe := filepath.Join(t.TempDir(), "murmur")
	_ = os.WriteFile(exe, []byte("current"), 0o755)
	_ = os.WriteFile(exe+".prev", []byte("older"), 0o755)
	_ = os.WriteFile(exe+".new", []byte("new"), 0o755)
	if err := Install(exe); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe + ".prev"); string(b) != "current" {
		t.Fatalf("murmur.prev holds %q", b)
	}
	// without a new binary, nothing changes
	exe = filepath.Join(t.TempDir(), "murmur")
	_ = os.WriteFile(exe, []byte("current"), 0o755)
	if err := Install(exe); err == nil {
		t.Fatal("installed nothing")
	}
	if b, _ := os.ReadFile(exe); string(b) != "current" {
		t.Fatalf("murmur holds %q", b)
	}
	// a murmur that is a link is replaced as a link: what it points to stays as it was
	dir := t.TempDir()
	target := filepath.Join(dir, "murmur-real")
	_ = os.WriteFile(target, []byte("current"), 0o755)
	exe = filepath.Join(dir, "murmur")
	_ = os.Symlink(target, exe)
	_ = os.WriteFile(exe+".new", []byte("new"), 0o755)
	if err := Install(exe); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "current" {
		t.Fatalf("the link's target holds %q", b)
	}
	if err := Restore(exe); err != nil {
		t.Fatal(err)
	}
	if l, err := os.Readlink(exe); err != nil || l != target {
		t.Fatalf("restored %q %v", l, err)
	}
}

// A folder this laptop can't write to stops the update before anything moves.
func TestDownloadIntoAReadOnlyFolder(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes anywhere")
	}
	defer func(v string) { release.Version = v }(release.Version)
	release.Version = "1.2.0"
	dir := t.TempDir()
	exe := filepath.Join(dir, "murmur")
	_ = os.WriteFile(exe, []byte("current"), 0o755)
	_ = os.Chmod(dir, 0o555)
	defer os.Chmod(dir, 0o755)
	bin := []byte("#!/bin/sh\necho 'murmur 1.3.0'\n")
	if _, err := download(serve(t, checksums(bin), nil, bin), exe); err == nil {
		t.Fatal("downloaded into a read-only folder")
	}
	if b, _ := os.ReadFile(exe); string(b) != "current" {
		t.Fatalf("murmur holds %q", b)
	}
}

// laptop is a laptop for Run: its own MURMUR_HOME, its murmur at exe printing version old, and a hub that says it runs
// claims and serves a binary printing version v. Each binary's daemon touches started and fails after wait seconds.
func laptop(t *testing.T, old, claims, v, wait string) (exe, started, hub string) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	dir := t.TempDir()
	exe, started = filepath.Join(dir, "murmur"), filepath.Join(dir, "started")
	script := func(v string) []byte {
		return []byte("#!/bin/sh\ncase \"$1\" in\nversion) echo 'murmur " + v + "' ;;\ndaemon) : > '" + started + "'; sleep " + wait + "; exit 1 ;;\nesac\n")
	}
	_ = os.WriteFile(exe, script(old), 0o755)
	was, version := installed, release.Version
	t.Cleanup(func() { installed, release.Version = was, version })
	installed, release.Version = func() string { return exe }, old
	bin := script(v)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dl/version", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"` + claims + `","updates":"auto"}`))
	})
	mux.HandleFunc("GET /dl/checksums.txt", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(checksums(bin)) })
	mux.HandleFunc("GET /dl/"+runtime.GOOS+"/"+runtime.GOARCH, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(bin) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return exe, started, srv.URL
}

// A new daemon that never comes online puts the previous binary back, and automatic updates skip that version.
func TestRunPutsThePreviousVersionBack(t *testing.T) {
	exe, started, hub := laptop(t, "1.2.0", "1.3.0", "1.3.0", "0")
	before, _ := os.ReadFile(exe)
	err := Run(config.Config{Hub: hub}, false, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "back on 1.2.0") {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(started); err != nil {
		t.Fatal("the new daemon never started")
	}
	if b, _ := os.ReadFile(exe); string(b) != string(before) {
		t.Fatalf("murmur is now %q", b)
	}
	if _, err := os.Stat(exe + ".new"); err == nil {
		t.Fatal("murmur.new was left behind")
	}
	if !config.UpdateSkipped("1.3.0") {
		t.Fatal("1.3.0 isn't skipped")
	}
	// within the hour, the daemon's update doesn't try it again; a person's does
	_ = os.Remove(started)
	if err := Run(config.Config{Hub: hub}, true, func(string) {}); err == nil || !strings.Contains(err.Error(), "less than an hour ago") {
		t.Fatalf("automatic update after a failure: %v", err)
	}
	if _, err := os.Stat(started); err == nil {
		t.Fatal("the automatic update tried 1.3.0 again")
	}
	_ = Run(config.Config{Hub: hub}, false, func(string) {})
	if _, err := os.Stat(started); err != nil {
		t.Fatal("murmur update by hand didn't try 1.3.0 again")
	}
}

// The skip goes by the version of the binary that failed, not by the version the hub says it runs: a hub whose
// binaries aren't its own version would otherwise have the laptop install and roll back the same binary over and
// over, each rollback restarting a daemon that starts the update again.
func TestSkipGoesByTheBinarysVersion(t *testing.T) {
	exe, started, hub := laptop(t, "1.2.0", "1.2.5", "1.3.0", "0")
	before, _ := os.ReadFile(exe)
	config.SkipUpdate("1.3.0")
	if err := Run(config.Config{Hub: hub}, true, func(string) {}); err == nil {
		t.Fatal("updated to a version that failed less than an hour ago")
	}
	if _, err := os.Stat(started); err == nil {
		t.Fatal("installed and started 1.3.0 again")
	}
	if b, _ := os.ReadFile(exe); string(b) != string(before) {
		t.Fatalf("murmur is now %q", b)
	}
	if _, err := os.Stat(exe + ".new"); err == nil {
		t.Fatal("murmur.new was left behind")
	}
}

// Two updates at once, such as the daemon's and one run by hand: the second waits for none and changes nothing, so
// murmur.prev still holds the version to go back to.
func TestOneUpdateAtATime(t *testing.T) {
	exe, started, hub := laptop(t, "1.2.0", "1.3.0", "1.3.0", "2")
	before, _ := os.ReadFile(exe)
	first := make(chan error, 1)
	go func() { first <- Run(config.Config{Hub: hub}, false, func(string) {}) }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break // the first one installed 1.3.0 and waits for its daemon
		}
		if time.Now().After(deadline) {
			t.Fatal("the first update never started its daemon")
		}
	}
	second := Run(config.Config{Hub: hub}, false, func(string) {})
	if err := <-first; err == nil || !strings.Contains(err.Error(), "back on 1.2.0") {
		t.Errorf("the first update: %v", err)
	}
	if second == nil {
		t.Error("the second update ran alongside the first")
	}
	if b, _ := os.ReadFile(exe); string(b) != string(before) {
		t.Fatalf("after both, murmur is %q", b)
	}
}

// What a hub says about its version reaches the terminal of whoever runs murmur update --check: no escape codes.
func TestCheckCleansTheHubsVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"1.0.0\u001b]0;owned\u0007\u001b[2J","updates":"auto"}`))
	}))
	defer srv.Close()
	i, err := Check(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsFunc(i.Version, func(r rune) bool { return r < ' ' }) {
		t.Fatalf("version %q", i.Version)
	}
}
