package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MURMUR_HUB as people set it, with a slash at the end: murmur adds its paths after it, and //ws is no path a hub
// serves.
func TestHubFromTheEnvironment(t *testing.T) {
	t.Setenv("MURMUR_HOME", t.TempDir())
	t.Setenv("MURMUR_HUB", "https://hub.acme.com//")
	if got := Load().Hub; got != "https://hub.acme.com" {
		t.Fatalf("hub %q", got)
	}
}

// A MURMUR_HOME that isn't absolute, such as "~/murmur" from a settings file no shell expands, names one folder for
// every process: the hooks, the daemon and the MCP server each run in a folder of their own.
func TestRelativeHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for in, want := range map[string]string{"~/murmur test": "murmur test", "~": "", "dev/m": "dev/m", "./m": "m"} {
		t.Setenv("MURMUR_HOME", in)
		if got := Dir(); got != filepath.Join(home, want) {
			t.Errorf("MURMUR_HOME=%q: %q", in, got)
		}
	}
	t.Setenv("MURMUR_HOME", "~/m")
	t.Chdir(t.TempDir())
	if err := Save(Config{Hub: "https://hub", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir()) // a hook in another project
	if got := Load(); got.Token != "t" {
		t.Fatalf("another folder's process doesn't find the config: %+v", got)
	}
}

// A MURMUR_HOME with spaces works, a config file murmur can't read is no config, and one it can't write says so.
func TestHomeOddities(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my murmur", "home")
	t.Setenv("MURMUR_HOME", dir)
	t.Setenv("MURMUR_HUB", "")
	if err := Save(Config{Hub: "https://hub", Token: "t"}); err != nil || Load().Token != "t" {
		t.Fatalf("save: %v, load %+v", err, Load())
	}
	if !strings.HasPrefix(SockPath(), dir) {
		t.Fatalf("socket %q", SockPath())
	}
	_ = os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"hub": "https://hub", "token": `), 0o600)
	if got := Load(); got.Token != "" {
		t.Fatalf("a config cut short: %+v", got)
	}
	long := filepath.Join(t.TempDir(), strings.Repeat("x", 120))
	t.Setenv("MURMUR_HOME", long)
	if s := SockPath(); len(s) >= 104 || !private(filepath.Dir(s)) {
		t.Fatalf("socket %q for a long home", s)
	}
	if os.Getuid() != 0 {
		ro := t.TempDir()
		_ = os.Chmod(ro, 0o500)
		t.Setenv("MURMUR_HOME", filepath.Join(ro, "m"))
		if err := Save(Config{}); err == nil {
			t.Fatal("saved into a folder it can't write")
		}
	}
}
