package setup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/EledoneAi/murmur/v2/internal/config"
)

// TestMain gives the tests a home folder, a murmur folder and coding tools' folders of their own. Some tests set a
// release version, which turns off the guards of a development build (guardDev, harness.Guard), and setup then
// writes wherever the coding tools keep their settings: without this, a developer's own ~/.codex, ~/.gemini,
// ~/.cursor, ~/.copilot and OpenCode's plugin get murmur's entries with the test binary's path, which go test
// deletes.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "murmur-setup-test-")
	if err != nil {
		panic(err)
	}
	for _, v := range append([]string{"HOME", "MURMUR_HOME"}, config.DirVars...) {
		dir := filepath.Join(root, v)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			panic(err)
		}
		_ = os.Setenv(v, dir)
	}
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}
