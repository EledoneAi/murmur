package gitscan

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRepoName(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:acme/api.git":        "acme/api",
		"https://github.com/acme/web":        "acme/web",
		"https://github.com/acme/tools.git/": "acme/tools",
		"":                                   "folder",
	} {
		if got := repoName(in, "/x/folder"); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestOverlapFilesSkipGeneratedAndData(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		pre := []string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@x", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}
		if out, err := exec.Command("git", append(pre, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	git("init", "-q")
	for p, c := range map[string]string{
		".gitattributes":          "traces/** murmur-ignore\napi.pb.go linguist-generated\n*.png binary\n",
		"server.go":               "package x\n",
		"traces/2026-09-16.jsonl": "{}\n",
		"api.pb.go":               "package x\n",
		"logo.png":                "png\n",
		"go.sum":                  "x\n",
		"web/package-lock.json":   "{}\n",
	} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-qm", "first") // no remote: every commit is work no remote has
	if got := strings.Join(Read(dir).Files, ","); got != ".gitattributes,server.go" {
		t.Fatalf("files: %s", got)
	}
	// new, untracked files: a data folder git would list as one "traces/" entry, and names with spaces and accents
	for p, c := range map[string]string{
		"traces/2026-10-14.jsonl": "{}\n", "données/é t.json": "{}\n", "notes/réunion 14.md": "x\n",
		".gitattributes": "traces/** murmur-ignore\napi.pb.go linguist-generated\n*.png binary\ndonnées/** murmur-ignore\n",
	} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(Read(dir).Files, ","); got != ".gitattributes,notes/réunion 14.md,server.go" {
		t.Fatalf("files with untracked folders: %s", got)
	}
	// hundreds of ignored data files must not push real work out of the 300-file list
	for i := range 350 {
		_ = os.WriteFile(filepath.Join(dir, "traces", fmt.Sprintf("run-%03d.jsonl", i)), []byte("{}\n"), 0o644)
	}
	_ = os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "src", "new_feature.go"), []byte("package src\n"), 0o644)
	if got := strings.Join(Read(dir).Files, ","); got != ".gitattributes,notes/réunion 14.md,server.go,src/new_feature.go" {
		t.Fatalf("files with many ignored ones: %s", got)
	}
}

func TestStatusPaths(t *testing.T) {
	out := "R  new.go\x00old.go\x00 R b.go\x00a.go\x00?? u v.txt\x00 M m.go\x00C  c2.go\x00c1.go\x00?? n.go\x00"
	paths, untracked := statusPaths(out)
	if got := strings.Join(paths, ","); got != "new.go,b.go,u v.txt,m.go,c2.go,n.go" {
		t.Fatalf("paths: %s", got)
	}
	if got := strings.Join(untracked, ","); got != "u v.txt,n.go" {
		t.Fatalf("untracked: %s", got)
	}
}

func TestCountNumstat(t *testing.T) {
	for _, c := range []struct {
		out                     string
		added, deleted, changed int
	}{
		{"", 0, 0, 0},
		{"3\t1\ta.go\x00", 3, 1, 1},
		{"3\t1\ta b.go\x00-\t-\tlogo.png\x000\t2\tc.go\x00", 3, 3, 3},
		{"1\t1\t\x00old name.go\x00new name.go\x004\t0\td.go\x00", 5, 1, 2}, // a rename, then a file
	} {
		if a, d, n := countNumstat(c.out); a != c.added || d != c.deleted || n != c.changed {
			t.Errorf("%q: +%d -%d %d files, want +%d -%d %d", c.out, a, d, n, c.added, c.deleted, c.changed)
		}
	}
}

func TestBaseOf(t *testing.T) {
	for _, c := range []struct{ log, current, def, base, head string }{
		{"a1\x00feat-2\nb2\x00\nc3\x00feat-1\nd4\x00main", "feat-2", "main", "feat-1", "a1"},
		{"a1\x00feat-2, main\nb2\x00", "feat-2", "main", "main", "a1"},           // just made from main
		{"a1\x00feat-2, main, feat-1\nb2\x00", "feat-2", "main", "feat-1", "a1"}, // from feat-1, itself at main
		{"a1\x00HEAD -> feat-2\nb2\x00old, main", "feat-2", "main", "old", "a1"}, // older git names HEAD
		{"a1\x00\nb2\x00feat-1", "detached", "main", "feat-1", "a1"},             // a detached HEAD, as in a rebase
		{"a1\x00feat-2\nb2\x00\nc3\x00", "feat-2", "main", "", "a1"},             // nothing below
		{"a1\x00main, feat-x\nb2\x00feat-y", "main", "main", "feat-x", "a1"},     // Read drops it on main
	} {
		if base, head := baseOf(c.log, c.current, c.def); base != c.base || head != c.head {
			t.Errorf("%q: base %q head %q, want %q %q", c.log, base, head, c.base, c.head)
		}
	}
}

func TestOpOf(t *testing.T) {
	for _, c := range []struct {
		files map[string]string
		want  string
	}{
		{nil, ""},
		{map[string]string{"rebase-merge/msgnum": "3\n", "rebase-merge/end": "7\n", "rebase-merge/head-name": "refs/heads/feat"}, "rebasing 3/7"},
		{map[string]string{"rebase-apply/next": "2\n", "rebase-apply/last": "5\n"}, "rebasing 2/5"},
		{map[string]string{"rebase-merge/interactive": ""}, "rebasing"},
		{map[string]string{"MERGE_HEAD": "abc\n"}, "merging"},
		{map[string]string{"CHERRY_PICK_HEAD": "abc\n"}, "cherry-picking"},
		{map[string]string{"REVERT_HEAD": "abc\n"}, "reverting"},
	} {
		dir := t.TempDir()
		for p, s := range c.files {
			_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
			_ = os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644)
		}
		if got := opOf(dir); got != c.want {
			t.Errorf("%v: %q, want %q", c.files, got, c.want)
		}
	}
}

// newRepo makes an empty git repo and returns it with a function that runs git in it.
func newRepo(t testing.TB) (string, func(args ...string) string) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		pre := []string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@x", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}
		out, err := exec.Command("git", append(pre, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	return dir, git
}

func write(t testing.TB, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A branch stacked on another counts what it would merge into the default branch: its commits, its staged and
// unstaged changes and its new files, binary ones as files without lines.
func TestState(t *testing.T) {
	dir, git := newRepo(t)
	write(t, dir, map[string]string{"a.go": "one\ntwo\nthree\n", "pkg/x.go": "package pkg\n"})
	git("add", "-A")
	git("commit", "-qm", "first")
	git("switch", "-qc", "feat-1")
	write(t, dir, map[string]string{"b.go": "b\nb\n"})
	git("add", "-A")
	git("commit", "-qm", "b")
	git("switch", "-qc", "feat-2")
	write(t, dir, map[string]string{"a.go": "one\n2\nthree\n", "logo.png": "\x00png"})
	git("add", "-A")
	git("commit", "-qm", "a and a logo")
	write(t, dir, map[string]string{"b.go": "b\nb\nb\n", "notes/todo.md": "1\n2\n3\n4", "data.bin": "\x00\x01"})
	git("add", "notes/todo.md") // staged, new
	write(t, dir, map[string]string{"scratch.txt": "x\ny\n"})

	r := Read(filepath.Join(dir, "pkg"))
	want := State{Added: 1 + 3 + 4 + 2, Deleted: 1, Changed: 6, Base: "feat-1", Dir: "pkg"} // a b logo todo; data scratch untracked
	if r.State != want {
		t.Fatalf("feat-2: %+v, want %+v", r.State, want)
	}
	git("restore", "b.go")
	git("switch", "-q", "main")
	if r := Read(dir); r.Base != "" || r.Dir != "" || r.Changed != 3 || r.Added != 6 { // what the tree still holds: todo, data, scratch
		t.Fatalf("main: %+v", r.State)
	}
	git("switch", "-q", "--detach", "feat-1")
	if r := Read(dir); r.Branch != "detached" || r.Detached != git("rev-parse", "--short", "HEAD") || r.Base != "feat-1" {
		t.Fatalf("detached: branch %q %+v", r.Branch, r.State)
	}
}

// wrapGit puts a git on PATH that runs the real one, except as the files in its folder say: "fail" names a subcommand
// that fails, "slow" a folder whose calls first wait two seconds.
func wrapGit(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	bin := t.TempDir()
	script := `#!/bin/sh
d=$(dirname "$0")
if [ -f "$d/fail" ]; then case " $* " in *" $(cat "$d/fail") "*) exit 1;; esac; fi
if [ -f "$d/slow" ]; then case " $* " in *" $(cat "$d/slow") "*) sleep 2;; esac; fi
exec '` + real + `' "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin
}

// A read in which a git call failed isn't kept for the next one, which reads the checkout again in full.
func TestFailedReadIsNotKept(t *testing.T) {
	dir, git := newRepo(t)
	write(t, dir, map[string]string{".gitattributes": "gen.go linguist-generated\n", "a.go": "a\n", "gen.go": "g\n"})
	git("add", "-A")
	git("commit", "-qm", "first")
	git("switch", "-qc", "feat")
	write(t, dir, map[string]string{"b.go": "b\n"})
	git("add", "-A")
	git("commit", "-qm", "b")
	write(t, dir, map[string]string{"a.go": "a\na\n", "gen.go": "g\ng\n"})
	want := Read(dir)
	bin := wrapGit(t)
	for i, cmd := range []string{"check-attr", "log", "diff"} {
		_ = os.WriteFile(filepath.Join(bin, "fail"), []byte(cmd), 0o644)
		at := time.Now().Add(time.Duration(i+1) * time.Second) // a.go as it was, at another time: the read can't hold
		_ = os.Chtimes(filepath.Join(dir, "a.go"), at, at)
		if r := Read(dir); reflect.DeepEqual(r, want) {
			t.Fatalf("with %s failing, the read is whole: %+v", cmd, r)
		}
		_ = os.Remove(filepath.Join(bin, "fail"))
		if r := Read(dir); !reflect.DeepEqual(r, want) {
			t.Fatalf("after %s failed: %+v, want %+v", cmd, r, want)
		}
	}
}

// An origin added to a checkout, or to the main checkout of a linked worktree, names its repo at the next read.
func TestOriginAddedShowsAtOnce(t *testing.T) {
	dir, git := newRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "first")
	wt := filepath.Join(t.TempDir(), "wt")
	git("worktree", "add", "-q", "-b", "feat", wt)
	if r := Read(dir); r.Repo != filepath.Base(dir) {
		t.Fatalf("without an origin: %q", r.Repo)
	}
	Read(wt)
	git("remote", "add", "origin", "git@github.com:acme/web.git")
	if r := Read(dir); r.Repo != "acme/web" {
		t.Fatalf("after git remote add: %q", r.Repo)
	}
	if r := Read(wt); r.Repo != "acme/web" {
		t.Fatalf("in the worktree, after git remote add: %q", r.Repo)
	}
}

// Reading a checkout that hasn't moved since starts git twice, and a read still sees every change: more lines in a
// file already changed, a branch below renamed, a commit.
func TestReadAgain(t *testing.T) {
	dir, git := newRepo(t)
	write(t, dir, map[string]string{"a.go": "one\n"})
	git("add", "-A")
	git("commit", "-qm", "first")
	git("switch", "-qc", "feat-1")
	git("switch", "-qc", "feat-2")
	write(t, dir, map[string]string{"a.go": "one\ntwo\n"})
	if r := Read(dir); r.Added != 1 || r.Base != "feat-1" {
		t.Fatalf("first read: %+v", r.State)
	}
	if n := spawns(t, func() { Read(dir) }); n != 2 {
		t.Fatalf("read again: git started %d times", n)
	}
	write(t, dir, map[string]string{"a.go": "one\ntwo\nthree\n"}) // git status still says " M a.go"
	if r := Read(dir); r.Added != 2 {
		t.Fatalf("after more lines: %+v", r.State)
	}
	git("branch", "-m", "feat-1", "feat-0")
	if r := Read(dir); r.Base != "feat-0" {
		t.Fatalf("after a rename: %+v", r.State)
	}
	git("commit", "-qam", "two")
	if r := Read(dir); r.Added != 2 || r.Changed != 1 || r.Base != "feat-0" || len(r.Files) != 1 {
		t.Fatalf("after a commit: %+v, files %v", r.State, r.Files)
	}
}

// A worktree keeps its rebase in its own git folder, not in the main checkout's .git.
func TestOpInAWorktree(t *testing.T) {
	dir, git := newRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "first")
	wt := filepath.Join(t.TempDir(), "wt")
	git("worktree", "add", "-q", "-b", "feat", wt)
	gitDir := strings.TrimSpace(func() string {
		out, _ := exec.Command("git", "-C", wt, "rev-parse", "--absolute-git-dir").Output()
		return string(out)
	}())
	write(t, gitDir, map[string]string{"rebase-merge/msgnum": "3\n", "rebase-merge/end": "7\n"})
	if r := Read(wt); r.Op != "rebasing 3/7" || r.Branch != "feat" {
		t.Fatalf("worktree: branch %q op %q", r.Branch, r.Op)
	}
	if r := Read(dir); r.Op != "" {
		t.Fatalf("main checkout: op %q", r.Op)
	}
}

func TestRepoOf(t *testing.T) {
	dir, git := newRepo(t)
	write(t, dir, map[string]string{"web/app.ts": "x\n"})
	if got := RepoOf(filepath.Join(dir, "web")); got != filepath.Base(dir) {
		t.Fatalf("without an origin: %q", got)
	}
	git("remote", "add", "origin", "git@github.com:acme/web.git")
	for _, p := range []string{dir, filepath.Join(dir, "web"), filepath.Join(dir, "web", "app.ts")} {
		if got := RepoOf(p); got != "acme/web" {
			t.Fatalf("%s: %q", p, got)
		}
	}
	if got := RepoOf(t.TempDir()); got != "" {
		t.Fatalf("outside git: %q", got)
	}
	if got := RepoOf(filepath.Join(dir, "gone")); got != "" {
		t.Fatalf("a folder that is gone: %q", got)
	}
}
