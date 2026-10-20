// Package update moves this laptop to the version its team's hub runs. Whether the daemon starts it on its own or a
// person runs `murmur update`, the old version checks the new one: it installs it, starts its daemon, and puts
// itself back if that daemon doesn't reach the hub within a minute.
package update

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/ipc"
	"github.com/EledoneAi/murmur/v2/internal/release"
)

// Info is what a hub says about the version its team runs.
type Info struct {
	Version    string `json:"version"`
	MinVersion string `json:"min_version,omitempty"`
	Updates    string `json:"updates"`
}

var client = &http.Client{Timeout: 2 * time.Minute}

// Check asks the hub which version the team runs.
func Check(hub string) (Info, error) {
	var i Info
	resp, err := client.Get(hub + "/dl/version")
	if err != nil {
		return i, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return i, fmt.Errorf("the hub answered %s", resp.Status)
	}
	return i, json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&i)
}

// Newer says whether v is a release after this one. Builds from source never update.
func Newer(v string) bool { return !release.Dev() && v != "" && release.Older(release.Version, v) }

// Managed names the package manager that installed exe, which updates it instead of murmur, or "".
func Managed(exe string) string {
	if r, err := filepath.EvalSymlinks(exe); err == nil { // a package manager's link on PATH points into its folders
		exe = r
	}
	for _, p := range []string{"/opt/homebrew/", "/usr/local/Cellar/", "/usr/local/Caskroom/", "/home/linuxbrew/", "/nix/store/"} {
		if strings.HasPrefix(exe, p) {
			if p == "/nix/store/" {
				return "Nix"
			}
			return "Homebrew"
		}
	}
	return ""
}

func binaryName() string { return "murmur-" + runtime.GOOS + "-" + runtime.GOARCH }

// Fetch downloads this laptop's binary from the hub, and checks it against the release's checksums, which the
// project's key must sign when this build knows the key. It never returns a binary that failed a check.
func Fetch(hub string) ([]byte, error) {
	get := func(path string, max int64) ([]byte, error) {
		resp, err := client.Get(hub + path)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: the hub answered %s", path, resp.Status)
		}
		return io.ReadAll(io.LimitReader(resp.Body, max))
	}
	sums, err := get("/dl/checksums.txt", 1<<16)
	if err != nil {
		return nil, err
	}
	sig, err := get("/dl/checksums.txt.sig", 1<<12)
	if err != nil && release.Key != "" {
		return nil, err
	}
	if !release.Signed(sums, string(sig)) {
		return nil, errors.New("the release's checksums are not signed with the project's key")
	}
	want := sumOf(sums, binaryName())
	if want == "" {
		return nil, fmt.Errorf("the release has no %s", binaryName())
	}
	body, err := get("/dl/"+runtime.GOOS+"/"+runtime.GOARCH, 128<<20)
	if err != nil {
		return nil, err
	}
	if got := sha256.Sum256(body); hex.EncodeToString(got[:]) != want {
		return nil, errors.New("the download doesn't match the release's checksum")
	}
	return body, nil
}

// sumOf finds a file's SHA-256 in a checksums.txt ("<hex>  <name>" per line).
func sumOf(sums []byte, name string) string {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		if sum, file, ok := strings.Cut(sc.Text(), "  "); ok && file == name {
			return sum
		}
	}
	return ""
}

// download fetches the hub's binary and writes it next to exe as exe.new, where Install finds it, and returns its
// version. The release's checksums say the binary is genuine, not which version it is, so the binary is asked, and
// one that isn't newer than this one is refused: a hub can't take laptops back to a version with a known bug.
func download(hub, exe string) (string, error) {
	body, err := Fetch(hub)
	if err != nil {
		return "", err
	}
	tmp := exe + ".new"
	if err := os.WriteFile(tmp, body, 0o755); err != nil {
		return "", err
	}
	v, err := versionOf(tmp)
	if err == nil && !Newer(v) {
		err = fmt.Errorf("the hub's binary is %s, not newer than this one (%s): murmur never goes back", v, release.Version)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return v, nil
}

var versionLine = regexp.MustCompile(`^murmur (v?[0-9]+\.[0-9]+\.[0-9]+\S*)$`)

// versionOf runs a binary's version command, which prints "murmur 1.2.3".
func versionOf(bin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version").Output()
	if err != nil {
		return "", fmt.Errorf("the new binary doesn't run on this laptop: %w", err)
	}
	m := versionLine.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		return "", fmt.Errorf("the new binary says it is %.40q, not a version", out)
	}
	return m[1], nil
}

// Install puts exe.new, which download wrote, in place of exe, keeping the binary it replaces as exe.prev. Each step
// is a rename, so exe is always a whole binary.
func Install(exe string) error {
	tmp := exe + ".new"
	if err := os.Rename(exe, exe+".prev"); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, exe); err != nil {
		_ = os.Rename(exe+".prev", exe)
		return err
	}
	return nil
}

// Restore puts the previous binary back.
func Restore(exe string) error { return os.Rename(exe+".prev", exe) }

// healthy is how long a new daemon has to reach the hub before the old version comes back.
var healthy = time.Minute

// Run updates this laptop to the hub's version: download and check it, install it, restart the daemon on it, and
// put the previous version back if the new daemon isn't online within a minute. say reports each step.
func Run(cfg config.Config, auto bool, say func(string)) error {
	if cfg.Hub == "" {
		return config.ErrNoHub
	}
	exe := config.Exe()
	if pm := Managed(exe); pm != "" {
		return fmt.Errorf("%s installed this murmur: update it with %s", pm, map[string]string{"Homebrew": "brew upgrade murmur", "Nix": "your Nix configuration"}[pm])
	}
	if cfg.Solo {
		return errors.New("murmur solo has no team to update with: install the newest release from github.com/EledoneAi/murmur/releases")
	}
	info, err := Check(cfg.Hub)
	if err != nil {
		return fmt.Errorf("asking the hub: %w", err)
	}
	if !Newer(info.Version) {
		say("Murmur " + release.Version + " is the team's version or newer: nothing to do")
		return nil
	}
	if auto && config.UpdateSkipped(info.Version) {
		return fmt.Errorf("%s failed on this laptop less than an hour ago", info.Version)
	}
	say("Downloading " + info.Version + " from " + cfg.Hub)
	v, err := download(cfg.Hub, exe)
	if err != nil {
		return err
	}
	if err := Install(exe); err != nil {
		return fmt.Errorf("installing: %w", err)
	}
	say("Installed " + v + ": restarting the daemon")
	if err := restart(exe, v); err != nil {
		say("The new daemon didn't come online (" + err.Error() + "): going back to " + release.Version)
		config.SkipUpdate(v) // the daemon doesn't try it again for a while, people can
		// the old binary goes back first, so a daemon started from exe from now on, by anyone, is the old version
		if rerr := Restore(exe); rerr != nil {
			return fmt.Errorf("%v, and %s could not be put back: %v", err, release.Version, rerr)
		}
		_ = stopDaemon()
		_ = ipc.StartDaemon(exe)
		return fmt.Errorf("%s didn't work on this laptop; back on %s", v, release.Version)
	}
	say("Murmur " + v + " is online")
	return nil
}

// restart stops the running daemon, starts the one installed at exe, and waits for it to be online on version v. It
// names exe: on Linux this program's own binary is the old version, now exe.prev.
func restart(exe, v string) error {
	if err := stopDaemon(); err != nil {
		return err
	}
	if err := ipc.StartDaemon(exe); err != nil {
		return err
	}
	deadline := time.Now().Add(healthy)
	var r ipc.Reply
	for time.Now().Before(deadline) {
		// never starting a daemon: one that stopped on its own is a failed update
		if ipc.CallRunning(ipc.Request{Op: "status"}, &r, 2*time.Second) == nil && r.State == "online" && r.Version == v {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	if r.State == "" {
		return errors.New("it didn't answer")
	}
	return errors.New("it stayed " + r.State)
}

// stopDaemon asks the daemon to exit and waits until its socket is gone.
func stopDaemon() error {
	c, err := ipc.Dial()
	if err != nil {
		return nil // not running
	}
	_ = json.NewEncoder(c).Encode(ipc.Request{Op: "stop"})
	c.Close()
	for range 100 {
		c, err := ipc.Dial()
		if err != nil {
			return nil
		}
		c.Close()
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("the running daemon didn't stop")
}
