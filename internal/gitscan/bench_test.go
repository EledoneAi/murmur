package gitscan

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// What one read of a checkout costs, in time, in git processes and in their CPU, as the daemon reads a working
// session's every 20 seconds: go test ./internal/gitscan -run '^$' -bench . -benchmem

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
	git("status") // refreshes the index, as the person's own git commands do: a fresh one makes every status hash files
	return dir
}

// A read of the checkout as it was at the last one, as most of the daemon's are, and a read after a file changed.
func BenchmarkRead(b *testing.B) {
	dir := benchCheckout(b)
	Read(dir)
	b.Run("unchanged", func(b *testing.B) {
		n := spawns(b, func() { Read(dir) })
		b.ReportAllocs()
		cpu := gitCPU()
		for b.Loop() {
			Read(dir)
		}
		b.ReportMetric(float64(n), "git/op")
		b.ReportMetric(float64(gitCPU()-cpu)/float64(time.Millisecond)/float64(b.N), "git-cpu-ms/op")
	})
	b.Run("changed", func(b *testing.B) {
		i := 0
		edit := func() {
			i++
			write(b, dir, map[string]string{"pkg01/file0001.go": strings.Repeat("edited\n", 30+i%2)})
		}
		edit()
		n := spawns(b, func() { Read(dir) })
		b.ReportAllocs()
		cpu := gitCPU()
		for b.Loop() {
			edit()
			Read(dir)
		}
		b.ReportMetric(float64(n), "git/op")
		b.ReportMetric(float64(gitCPU()-cpu)/float64(time.Millisecond)/float64(b.N), "git-cpu-ms/op")
	})
}

// gitCPU is the CPU time of the processes this one started and waited for.
func gitCPU() time.Duration {
	var u syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_CHILDREN, &u)
	return time.Duration(syscall.TimevalToNsec(u.Utime) + syscall.TimevalToNsec(u.Stime))
}

// spawns counts the git processes f starts, through a git on PATH that notes each one.
func spawns(b testing.TB, f func()) int {
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
