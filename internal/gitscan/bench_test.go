package gitscan

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// What one read of a checkout costs, in time and in git processes, as the daemon reads a working session's every 20
// seconds: go test ./internal/gitscan -run '^$' -bench . -benchmem

// benchCheckout is a checkout of 2,000 files pushed to its origin, on a branch with five commits of its own, ten
// files changed in the tree, and new files: five of source and a 3 MB data dump.
func benchCheckout(b *testing.B) string {
	b.Helper()
	origin := filepath.Join(b.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", origin).CombinedOutput(); err != nil {
		b.Fatalf("%s", out)
	}
	dir, git := newRepo(b)
	files := map[string]string{}
	for i := range 2000 {
		files[fmt.Sprintf("pkg%02d/file%04d.go", i%40, i)] = strings.Repeat("line\n", 50)
	}
	write(b, dir, files)
	git("add", "-A")
	git("commit", "-qm", "first")
	git("remote", "add", "origin", origin)
	git("push", "-q", "origin", "main")
	git("remote", "set-head", "origin", "main")
	git("switch", "-qc", "feat/bench")
	for c := range 5 {
		write(b, dir, map[string]string{fmt.Sprintf("pkg00/file%04d.go", c*40): strings.Repeat("changed\n", 20)})
		git("commit", "-qam", fmt.Sprint("change ", c))
	}
	for i := range 10 {
		write(b, dir, map[string]string{fmt.Sprintf("pkg01/file%04d.go", 1+i*40): strings.Repeat("edited\n", 30)})
	}
	for i := range 5 {
		write(b, dir, map[string]string{fmt.Sprintf("new/n%d.go", i): strings.Repeat("new line\n", 200)})
	}
	write(b, dir, map[string]string{"data/dump.csv": strings.Repeat("1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16\n", 80_000)})
	return dir
}

func BenchmarkRead(b *testing.B) {
	dir := benchCheckout(b)
	n := spawns(b, func() { Read(dir) })
	b.ReportAllocs()
	for b.Loop() {
		Read(dir)
	}
	b.ReportMetric(float64(n), "git/op")
}

// spawns counts the git processes f starts, through a git on PATH that notes each one.
func spawns(b *testing.B, f func()) int {
	b.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		b.Skip("no git")
	}
	bin, log := b.TempDir(), filepath.Join(b.TempDir(), "spawns")
	script := "#!/bin/sh\necho >> '" + log + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		b.Fatal(err)
	}
	path := os.Getenv("PATH")
	b.Setenv("PATH", bin+string(os.PathListSeparator)+path)
	f()
	b.Setenv("PATH", path)
	data, _ := os.ReadFile(log)
	return strings.Count(string(data), "\n")
}
