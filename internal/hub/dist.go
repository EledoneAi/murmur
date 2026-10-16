package hub

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/EledoneAi/murmur/internal/release"
)

// The hub serves the laptop binaries of its own release: the installer downloads one, and laptops update to it.
// They come from Config.Dist, a folder with murmur-<os>-<arch> files and the release's checksums.txt and
// checksums.txt.sig.

var (
	distName = regexp.MustCompile(`^murmur-(darwin|linux)-(arm64|amd64)$`)
	distMeta = map[string]bool{"checksums.txt": true, "checksums.txt.sig": true}
)

const installScript = `#!/bin/sh
# Murmur installer: one binary in ~/.local/bin, then ` + "`murmur join`" + `.
set -eu
HUB="%s"
os=$(uname -s | tr 'A-Z' 'a-z')
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) echo "murmur: unsupported CPU $(uname -m)" >&2; exit 1 ;; esac
case "$os" in darwin|linux) ;; *) echo "murmur: macOS and Linux only; on Windows, run this inside WSL, where Claude Code runs too" >&2; exit 1 ;; esac
dir="$HOME/.local/bin"
mkdir -p "$dir"
tmp=$(mktemp "$dir/.murmur.XXXXXX")
trap 'rm -f "$tmp"' EXIT
curl -fsSL "$HUB/dl/$os/$arch" -o "$tmp"
want=$(curl -fsSL "$HUB/dl/checksums.txt" | awk -v f="murmur-$os-$arch" '$2 == f { print $1 }')
got=$( (command -v sha256sum >/dev/null && sha256sum "$tmp" || shasum -a 256 "$tmp") | awk '{ print $1 }')
if [ -z "$want" ] || [ "$want" != "$got" ]; then echo "murmur: the download doesn't match the release's checksums" >&2; exit 1; fi
chmod 755 "$tmp"
mv -f "$tmp" "$dir/murmur"
trap - EXIT
case ":$PATH:" in *":$dir:"*) ;; *) echo "Add $dir to your PATH to run murmur." ;; esac
"$dir/murmur" join "$HUB"
`

func (h *Hub) install(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	fmt.Fprintf(w, installScript, h.cfg.PublicURL)
}

func (h *Hub) download(w http.ResponseWriter, r *http.Request) {
	name := "murmur-" + r.PathValue("os") + "-" + r.PathValue("arch")
	if h.cfg.Dist == nil || !distName.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	if ip := h.clientIP(r); !h.gets.allow(ip, h.Settings().DownloadsPerHour) {
		h.log.Info("download refused", "address", ip, "why", "too many from this address")
		http.Error(w, "too many downloads from this address; try again in an hour", http.StatusTooManyRequests)
		return
	}
	f, err := h.cfg.Dist.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	sum, err2 := h.checksum(name)
	if err != nil || err2 != nil {
		http.Error(w, "this download is not available right now", http.StatusInternalServerError)
		return
	}
	// always the whole file, which is what the checksum covers: no ranges
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.Header().Set("X-Sha256", sum)
	w.Header().Set("X-Version", h.cfg.Version)
	_, _ = io.Copy(w, f) // streamed: a download never holds the binary in memory
}

// version tells laptops and `murmur update` which version the team runs, and what the team asks of them.
func (h *Hub) version(w http.ResponseWriter, _ *http.Request) {
	s := h.Settings()
	writeJSON(w, http.StatusOK, map[string]string{"version": h.cfg.Version, "min_version": s.MinVersion, "updates": s.UpdatePolicy})
}

// CheckDist refuses client binaries the project's key doesn't sign, when this build knows the key: laptops would
// refuse every update and stay on their version while the hub moves on. The hub fails to start instead.
func CheckDist(dist fs.FS) error {
	if release.Key == "" || dist == nil {
		return nil
	}
	sums, err := fs.ReadFile(dist, "checksums.txt")
	if err != nil {
		return errors.New("DIST_DIR has no checksums.txt")
	}
	sig, _ := fs.ReadFile(dist, "checksums.txt.sig")
	if !release.Signed(sums, string(sig)) {
		return errors.New("DIST_DIR's checksums.txt is not signed with the project's release key")
	}
	return nil
}

// distFile serves the release's checksums and their signature, which laptops check before installing.
func (h *Hub) distFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if h.cfg.Dist == nil || !distMeta[name] {
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(h.cfg.Dist, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(b)
}

// checksum is a binary's SHA-256: from the release's checksums.txt, or computed once from the file.
func (h *Hub) checksum(name string) (string, error) {
	if s, ok := h.sums.Load(name); ok {
		return s.(string), nil
	}
	if f, err := h.cfg.Dist.Open("checksums.txt"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if sum, file, ok := strings.Cut(sc.Text(), "  "); ok && file == name {
				f.Close()
				h.sums.Store(name, sum)
				return sum, nil
			}
		}
		f.Close()
	}
	f, err := h.cfg.Dist.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	s := hex.EncodeToString(sum.Sum(nil))
	h.sums.Store(name, s)
	return s, nil
}

// clientIP is the address a request came from. Behind a proxy (TrustProxy) that is the last X-Forwarded-For hop,
// which the proxy appended; hops before it are whatever the client wrote.
func (h *Hub) clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); h.cfg.TrustProxy && f != "" {
		if ip, err := netip.ParseAddr(strings.TrimSpace(f[strings.LastIndex(f, ",")+1:])); err == nil {
			return ip.String()
		}
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// downloads counts recent downloads per address, on this instance.
type downloads struct {
	mu   sync.Mutex
	seen map[string][]time.Time
}

// maxAddresses bounds the addresses downloads remembers: past it, the oldest hour's record is forgotten early.
const maxAddresses = 100_000

func (d *downloads) allow(ip string, perHour int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, known := d.seen[ip]; !known && len(d.seen) >= maxAddresses {
		clear(d.seen) // ponytail: forgets everyone at once; an LRU if one address flood ever matters
	}
	now := time.Now()
	recent := slices.DeleteFunc(d.seen[ip], func(t time.Time) bool { return now.Sub(t) > time.Hour })
	if len(recent) >= perHour {
		d.seen[ip] = recent
		return false
	}
	d.seen[ip] = append(recent, now)
	return true
}

func (d *downloads) sweep() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for ip, ts := range d.seen {
		if len(ts) == 0 || time.Since(ts[len(ts)-1]) > time.Hour {
			delete(d.seen, ip)
		}
	}
}
