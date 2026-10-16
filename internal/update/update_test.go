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
	"testing"
	"time"

	"github.com/EledoneAi/murmur/internal/release"
)

// serve is a hub with one release: its checksums, their signature and this laptop's binary.
func serve(t *testing.T, sums, sig, body []byte) string {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dl/checksums.txt", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(sums) })
	mux.HandleFunc("GET /dl/checksums.txt.sig", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(sig) })
	mux.HandleFunc("GET /dl/"+runtime.GOOS+"/"+runtime.GOARCH, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Version", "9.9.9")
		_, _ = w.Write(body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func checksums(bin []byte) []byte {
	sum := sha256.Sum256(bin)
	return []byte(hex.EncodeToString(sum[:]) + "  " + binaryName() + "\n" + "00  murmur-plan9-mips\n")
}

// A download is installed only when it matches the release's checksums, and the checksums carry the project's
// signature when the build knows the project's key.
func TestFetch(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	bin := []byte("the new murmur")
	sums := checksums(bin)
	good := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sums)))

	release.Key = base64.StdEncoding.EncodeToString(pub)
	defer func() { release.Key = "" }()
	if body, err := Fetch(serve(t, sums, good, bin)); err != nil || string(body) != string(bin) {
		t.Fatalf("good release: %q %v", body, err)
	}
	if _, err := Fetch(serve(t, sums, []byte("bm90IGEgc2ln"), bin)); err == nil {
		t.Fatal("a bad signature was accepted")
	}
	if _, err := Fetch(serve(t, sums, good, []byte("something else"))); err == nil {
		t.Fatal("a binary that doesn't match its checksum was accepted")
	}
	release.Key = ""
	if _, err := Fetch(serve(t, sums, nil, bin)); err != nil {
		t.Fatalf("without a key, checksums are enough: %v", err)
	}
}

// A genuine release can still be an older one. The binary says which version it is, whatever the hub says, and
// only a newer one is kept.
func TestDownloadNeverGoesBack(t *testing.T) {
	defer func(v string) { release.Version = v }(release.Version)
	release.Version = "1.2.0"
	exe := filepath.Join(t.TempDir(), "murmur")
	for _, c := range []struct{ prints, want string }{
		{"murmur 1.3.0", "1.3.0"},
		{"murmur 1.1.0", ""},
		{"murmur 1.2.0", ""},
		{"murmur dev", ""},
		{"something else", ""},
	} {
		bin := []byte("#!/bin/sh\necho '" + c.prints + "'\n")
		v, err := download(serve(t, checksums(bin), nil, bin), exe)
		if v != c.want || (err == nil) != (c.want != "") {
			t.Fatalf("a binary printing %q: %q, %v", c.prints, v, err)
		}
		if _, err := os.Stat(exe + ".new"); (err == nil) != (c.want != "") {
			t.Fatalf("a binary printing %q: is %s.new there? %v", c.prints, exe, err == nil)
		}
	}
}

func TestInstallKeepsThePreviousVersion(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "murmur")
	_ = os.WriteFile(exe, []byte("old"), 0o755)
	_ = os.WriteFile(exe+".new", []byte("new"), 0o755)
	if err := Install(exe); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Fatalf("installed %q", b)
	}
	if err := Restore(exe); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatalf("restored %q", b)
	}
}

func TestNewerAndManaged(t *testing.T) {
	defer func(v string) { release.Version = v }(release.Version)
	release.Version = "dev"
	if Newer("9.9.9") {
		t.Fatal("a build from source updated itself")
	}
	release.Version = "1.2.0"
	if !Newer("1.3.0") || Newer("1.2.0") || Newer("1.1.9") || Newer("") {
		t.Fatal("wrong ordering")
	}
	if Managed("/opt/homebrew/Cellar/murmur/1.2.0/bin/murmur") != "Homebrew" || Managed("/Users/x/.local/bin/murmur") != "" {
		t.Fatal("package managers not recognised")
	}
}

// A version that failed is skipped, and named for status, for an hour.
func TestSkipped(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	if Skipped("1.3.0") || Failed() != "" {
		t.Fatal("skipped with no failure")
	}
	_ = os.WriteFile(skipped(), []byte("1.3.0"), 0o600)
	if !Skipped("1.3.0") || Skipped("1.4.0") || Skipped("") || Failed() != "1.3.0" {
		t.Fatal("a failure an instant ago")
	}
	old := time.Now().Add(-2 * skipFor)
	_ = os.Chtimes(skipped(), old, old)
	if Skipped("1.3.0") || Failed() != "" {
		t.Fatal("a failure two hours ago")
	}
}
