package gitscan

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A checkout's own git config runs nothing when murmur reads it: an agent may work in a folder its person downloaded,
// whose .git/config names commands for git to run, and murmur reads every working session's checkout on its own.
func TestHostileConfigRunsNothing(t *testing.T) {
	dir, git := newRepo(t)
	ran := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "pwn.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$0 $*\" >> '"+ran+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, map[string]string{"a.go": "one\n"})
	git("add", "-A")
	git("commit", "-qm", "first")
	// a commit with a signature, for log.showSignature to check with gpg.program
	obj := "tree " + git("rev-parse", "HEAD^{tree}") + "\nparent " + git("rev-parse", "HEAD") +
		"\nauthor t <t@x> 1700000000 +0000\ncommitter t <t@x> 1700000000 +0000\n" +
		"gpgsig -----BEGIN PGP SIGNATURE-----\n \n x\n -----END PGP SIGNATURE-----\n\nsigned\n"
	cmd := exec.Command("git", "-C", dir, "hash-object", "-t", "commit", "-w", "--stdin")
	cmd.Stdin = strings.NewReader(obj)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	git("update-ref", "HEAD", strings.TrimSpace(string(out)))
	for k, v := range map[string]string{"core.fsmonitor": script, "log.showSignature": "true", "gpg.program": script,
		"diff.external": script, "core.pager": script} {
		git("config", k, v)
	}
	_ = os.WriteFile(filepath.Join(dir, ".git", "hooks", "post-index-change"), []byte("#!/bin/sh\n'"+script+"' hook\n"), 0o755)
	write(t, dir, map[string]string{"a.go": "two\n", "b.go": "new\n"})
	r := Read(dir)
	if b, _ := os.ReadFile(ran); len(b) > 0 {
		t.Fatalf("the checkout's config ran:\n%s", b)
	}
	if r.Branch != "main" || strings.Join(r.Files, ",") != "a.go,b.go" || r.Changed != 2 {
		t.Fatalf("read: %+v", r)
	}
}

// A remote HEAD that names an option instead of a branch never reaches git as one: git refuses the link, and the
// default branch is found as without it.
func TestRemoteHeadThatNamesAnOption(t *testing.T) {
	dir, git := newRepo(t)
	write(t, dir, map[string]string{"a.go": "one\n"})
	git("add", "-A")
	git("commit", "-qm", "first")
	out := filepath.Join(t.TempDir(), "written")
	_ = os.MkdirAll(filepath.Join(dir, ".git", "refs", "remotes", "origin"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".git", "refs", "remotes", "origin", "HEAD"), []byte("ref: --output="+out+"\n"), 0o644)
	if ref, _, _ := defaultBranch(t.Context(), dir); ref != "refs/heads/main" {
		t.Fatalf("default branch %q", ref)
	}
	_ = Read(dir)
	if _, err := os.Stat(out); err == nil {
		t.Fatal("git wrote the file the remote HEAD named")
	}
}

// What isn't a checkout with a commit reads as no checkout: a plain folder, a bare repo, a .git file that points
// nowhere, a repo with no commit yet, a folder that is gone.
func TestNotACheckout(t *testing.T) {
	empty, _ := newRepo(t)
	bare := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatalf("%s", out)
	}
	dangling := t.TempDir()
	_ = os.WriteFile(filepath.Join(dangling, ".git"), []byte("gitdir: /nowhere/at/all\n"), 0o644)
	for _, dir := range []string{t.TempDir(), bare, dangling, empty, filepath.Join(empty, "gone")} {
		if r := Read(dir); r.Top != dir || r.Repo != filepath.Base(dir) || r.Tree != "" || r.Branch != "" || r.Files != nil {
			t.Errorf("%s: %+v", dir, r)
		}
	}
}

// A file whose name holds a newline is one file, as git names it.
func TestNewlineInAFileName(t *testing.T) {
	dir, git := newRepo(t)
	write(t, dir, map[string]string{"a.go": "a\n"})
	git("add", "-A")
	git("commit", "-qm", "first")
	write(t, dir, map[string]string{"new\nline.go": "x\n", "a.go": "b\n"})
	if r := Read(dir); strings.Join(r.Files, "|") != "a.go|new\nline.go" || r.Changed != 2 || r.Added != 2 {
		t.Fatalf("%q %+v", r.Files, r.State)
	}
}
