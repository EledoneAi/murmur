// Package gitscan reads what a checkout is and what it changes: repo, branch, the files no remote has yet, the size
// of its change and where it stands.
package gitscan

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Result is one checkout: its top folder, repo (owner/name from the origin, or the folder name), branch, a checkout id
// that sessions sharing the checkout share, and the files it changes that no remote has.
type Result struct {
	Top, Repo, Branch, Tree string
	Files                   []string
	State
}

// State is the size of a checkout's change against the merge base with the default branch, working tree included,
// and where the checkout stands: the nearest branch below HEAD, HEAD's short commit when it is detached, a rebase
// or merge in progress, and the folder it was read from, under the top.
type State struct {
	Added, Deleted, Changed int
	Base, Detached, Op, Dir string
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	// a checkout's own config may name commands: murmur reads checkouts on its own, so status and diff run no
	// fsmonitor hook and log checks no signature (gpg.program)
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "core.fsmonitor=false", "-c", "log.showSignature=false"}, args...)...)
	// never take the index lock: the developer's own git commands must not trip over ours
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	return strings.TrimRight(string(out), "\n"), err
}

var originRe = regexp.MustCompile(`[:/]([^/:]+/[^/]+?)(?:\.git)?/?$`)

// repoName gives "acme/api" for any GitHub-style origin URL, or the folder name.
func repoName(origin, top string) string {
	if m := originRe.FindStringSubmatch(strings.TrimSpace(origin)); m != nil {
		return m[1]
	}
	return filepath.Base(top)
}

var host, _ = os.Hostname()

// reading lets one Read run at a time. The daemon reads every working session's checkout on the same tick, and eight
// git processes started together took twice the CPU each that they took one after another.
var reading sync.Mutex

// Read reads a checkout from a folder in it, once the reads before it are done. Each read starts git twice: once for
// where the checkout is, the commits at HEAD and at every branch's tip and the local branches' names, once for git
// status. When those, and the size and time of each file status names, are as they were at the checkout's last read,
// the rest of that read holds; else four more calls read what changed.
func Read(cwd string) Result {
	reading.Lock()
	defer reading.Unlock()
	return scan(cwd)
}

// ReadNow is Read without waiting for the reads before it, for a session's first read: its brief waits for it, and
// another checkout's git can take seconds.
func ReadNow(cwd string) Result { return scan(cwd) }

func scan(cwd string) Result {
	ctx, done := context.WithTimeout(context.Background(), 8*time.Second)
	defer done()
	out, err := git(ctx, cwd, "rev-parse", "--show-toplevel", "--show-prefix", "--absolute-git-dir", "HEAD", "--branches", "--remotes",
		"--symbolic-full-name", "--branches", "--abbrev-ref", "HEAD")
	lines := strings.Split(out, "\n")
	if err != nil || len(lines) < 5 {
		return Result{Top: cwd, Repo: filepath.Base(cwd)}
	}
	r := Result{Top: lines[0], Branch: lines[len(lines)-1]}
	r.Dir, r.Op = strings.TrimSuffix(lines[1], "/"), opOf(lines[2])
	sum := sha256.Sum256([]byte(host + "\x00" + r.Top))
	r.Tree = hex.EncodeToString(sum[:6])
	origin := remoteOf(ctx, r.Top, lines[2])
	r.Repo = repoName(origin.url, r.Top)
	if r.Branch == "HEAD" {
		r.Branch = "detached"
	}
	status, err := git(ctx, r.Top, "status", "--porcelain", "-z", "--untracked-files=all")
	whole := err == nil // every call answered: only then may the read stand for the next ones
	if err != nil {
		status = ""
	}
	paths, untracked := statusPaths(status)
	key := keyOf(r.Top, []string{r.Branch, origin.ref, strings.Join(lines[3:len(lines)-1], " "), status}, paths)
	if last, ok := lastRead(r.Top, key); ok {
		r.Files, r.Added, r.Deleted, r.Changed, r.Base, r.Detached = last.Files, last.Added, last.Deleted, last.Changed, last.Base, last.Detached
		return r
	}

	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] && len(seen) < 5000 { // cut to 300 below, once lockfiles and ignored files are out
			seen[p] = true
			r.Files = append(r.Files, p)
		}
	}
	// What this checkout changes: commits no remote has yet, then the working tree below. Comparing with
	// origin/HEAD counted a shared branch's whole history (staging against main) as everyone's change.
	// ponytail: pushed commits drop out, so two pushed feature branches overlap only on what they still change.
	// -z gives paths as they are, without git's quoting of spaces and accents, and untracked folders come file by
	// file, so .gitattributes rules such as traces/** apply to them.
	log, err := git(ctx, r.Top, "log", "-z", "--max-count=200", "--format=", "--name-only", "HEAD", "--not", "--remotes")
	whole = whole && err == nil
	if err == nil {
		for p := range strings.SplitSeq(log, "\x00") {
			add(p)
		}
	}
	for _, p := range paths {
		add(p)
	}
	r.Files = slices.DeleteFunc(r.Files, func(p string) bool { return lockfiles[filepath.Base(p)] })
	r.Files, err = dropUnshared(ctx, r.Top, r.Files)
	whole = whole && err == nil
	r.Files = r.Files[:min(len(r.Files), 300)]
	slices.Sort(r.Files)

	// The size of the change counts what the branch would merge, so it is taken against the default branch, unlike
	// Files. Two more git calls: the diff, and a walk down HEAD's first parents for the branch below it.
	ref, def := origin.ref, origin.branch
	numstat, err := git(ctx, r.Top, "diff", "--numstat", "-z", "--no-ext-diff", "--no-textconv", "--merge-base", cmp.Or(ref, "HEAD"))
	whole = whole && err == nil
	if err == nil {
		r.Added, r.Deleted, r.Changed = countNumstat(numstat)
	}
	r.Added += untrackedLines(r.Top, untracked)
	r.Changed += len(untracked)
	log, err = git(ctx, r.Top, "log", "--first-parent", "--format=%h%x00%D", "--decorate-refs=refs/heads/", "-n", "500", "HEAD")
	whole = whole && err == nil
	if err == nil {
		base, head := baseOf(log, r.Branch, def)
		if r.Branch != def { // the default branch is stacked on nothing, whatever merged branches it holds
			r.Base = base
		}
		if r.Branch == "detached" {
			r.Detached = head
		}
	}
	if whole && ctx.Err() == nil {
		keep(r.Top, key, r)
	}
	return r
}

// statusPaths reads `git status --porcelain -z`: one "XY path" entry per file, where a rename or a copy, in either
// column, is followed by the path it came from. It returns every path, and the untracked ones again.
func statusPaths(out string) (paths, untracked []string) {
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		if e := entries[i]; len(e) > 3 {
			paths = append(paths, e[3:])
			if e[:2] == "??" {
				untracked = append(untracked, e[3:])
			}
			if strings.ContainsAny(e[:2], "RC") {
				i++
			}
		}
	}
	return paths, untracked
}

// countNumstat reads `git diff --numstat -z`: "added<TAB>deleted<TAB>path" per file, or "added<TAB>deleted<TAB>"
// followed by the two paths of a rename. A binary file has "-" for both counts, and counts as a file of no lines.
func countNumstat(out string) (added, deleted, files int) {
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		a, rest, ok := strings.Cut(entries[i], "\t")
		if !ok {
			continue
		}
		d, path, _ := strings.Cut(rest, "\t")
		if path == "" {
			i += 2
		}
		n, _ := strconv.Atoi(a)
		m, _ := strconv.Atoi(d)
		added, deleted, files = added+n, deleted+m, files+1
	}
	return added, deleted, files
}

// untrackedLines counts the lines of the new files git doesn't track yet, within a small budget of reading: they are
// often data or build output. Binary files, as git tells them (a NUL near the start), count none.
func untrackedLines(top string, paths []string) int {
	budget, n := int64(4<<20), 0
	buf := make([]byte, 32<<10)
	for _, p := range paths[:min(len(paths), 500)] {
		st, err := os.Lstat(filepath.Join(top, p))
		if err != nil || !st.Mode().IsRegular() || st.Size() > budget {
			continue
		}
		budget -= st.Size()
		n += linesOf(filepath.Join(top, p), buf)
	}
	return n
}

// linesOf counts a file's lines, reading it through buf, and none for a binary file.
func linesOf(path string, buf []byte) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n, last := 0, byte('\n')
	for first := true; ; first = false {
		k, err := io.ReadFull(f, buf)
		if first && bytes.IndexByte(buf[:min(k, 8000)], 0) >= 0 {
			return 0
		}
		n += bytes.Count(buf[:k], []byte{'\n'})
		if k > 0 {
			last = buf[k-1]
		}
		if err != nil {
			break
		}
	}
	if last != '\n' {
		n++
	}
	return n
}

// remotes keeps each checkout's origin and default branch for a while: they rarely change, and each is a git call.
var remotes sync.Map // top -> remote

type remote struct {
	url, ref, branch string // origin's URL; the default branch's ref and name
	config           string // the repo's config file as it was, its size and time: an origin added since shows at once
	at               time.Time
}

// remoteOf is the origin and default branch of the checkout at top, whose git folder is gitDir.
func remoteOf(ctx context.Context, top, gitDir string) remote {
	config := stamp(configOf(gitDir))
	if f, ok := remotes.Load(top); ok && f.(remote).config == config && time.Since(f.(remote).at) < 10*time.Minute {
		return f.(remote)
	}
	url, _ := git(ctx, top, "config", "--get", "remote.origin.url")
	ref, branch, ok := defaultBranch(ctx, top)
	o := remote{url: url, ref: ref, branch: branch, config: config, at: time.Now()}
	if ok {
		remotes.Store(top, o)
	}
	return o
}

// configOf is the config file of the repo whose git folder is gitDir: a linked worktree's is its main checkout's.
// ponytail: an origin set in a file the config includes shows within the ten minutes, not at once.
func configOf(gitDir string) string {
	if b, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		dir := strings.TrimSpace(string(b))
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(gitDir, dir)
		}
		return filepath.Join(dir, "config")
	}
	return filepath.Join(gitDir, "config")
}

// stamp is a file's size and modification time, or "" when it is missing.
func stamp(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return strconv.FormatInt(st.Size(), 10) + " " + strconv.FormatInt(st.ModTime().UnixNano(), 10)
}

// defaultBranch finds the branch the team merges into: origin's HEAD, else main or master on origin, else here. It
// returns the ref to diff against and the branch's name, or nothing when the repo has none of them, and false when
// git failed.
func defaultBranch(ctx context.Context, top string) (ref, name string, ok bool) {
	candidates := []string{"refs/remotes/origin/main", "refs/remotes/origin/master", "refs/heads/main", "refs/heads/master"}
	out, err := git(ctx, top, append([]string{"for-each-ref", "--format=%(refname)%00%(symref)", "refs/remotes/origin/HEAD"}, candidates...)...)
	if err != nil {
		return "", "", false
	}
	have := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		r, sym, _ := strings.Cut(line, "\x00")
		have[r] = sym
	}
	ref = have["refs/remotes/origin/HEAD"]
	for _, c := range candidates {
		if _, ok := have[c]; ok && ref == "" {
			ref = c
		}
	}
	name = strings.TrimPrefix(strings.TrimPrefix(ref, "refs/remotes/origin/"), "refs/heads/")
	return ref, name, true
}

// reads keeps each checkout's last read, which holds while nothing it was read from has moved, for ten minutes at
// most.
// ponytail: what only $GIT_DIR/info/attributes or git's config changes shows within those ten minutes, not at once.
var reads = struct {
	sync.Mutex
	m map[string]read // top ->
}{m: map[string]read{}}

type read struct {
	key [sha256.Size]byte
	r   Result
	at  time.Time
}

// keyOf sums up what a read of top depends on: these words (the branch, the default branch, the commits, what git
// status says), and the size and time of each file status names.
func keyOf(top string, words, paths []string) [sha256.Size]byte {
	var b []byte
	for _, w := range words {
		b = append(append(b, w...), 0)
	}
	for _, p := range paths {
		if st, err := os.Lstat(filepath.Join(top, p)); err == nil {
			b = strconv.AppendInt(append(strconv.AppendInt(b, st.Size(), 10), ' '), st.ModTime().UnixNano(), 10)
		}
		b = append(b, 0)
	}
	return sha256.Sum256(b)
}

// lastRead is the checkout's last read, if it was read from what key sums up.
func lastRead(top string, key [sha256.Size]byte) (Result, bool) {
	reads.Lock()
	defer reads.Unlock()
	last, ok := reads.m[top]
	if !ok || last.key != key || time.Since(last.at) > 10*time.Minute {
		return Result{}, false
	}
	last.r.Files = slices.Clone(last.r.Files)
	return last.r, true
}

func keep(top string, key [sha256.Size]byte, r Result) {
	reads.Lock()
	defer reads.Unlock()
	if len(reads.m) > 1000 {
		clear(reads.m)
	}
	r.Files = slices.Clone(r.Files)
	reads.m[top] = read{key: key, r: r, at: time.Now()}
}

// baseOf reads `git log --first-parent --format=%h%x00%D --decorate-refs=refs/heads/`, newest first: it returns the
// first branch on the way down other than the current one, and HEAD's short commit. Where several branches point at
// one commit, one other than the default branch wins: feat-2, just made from feat-1, is stacked on feat-1.
func baseOf(log, current, def string) (base, head string) {
	for i, line := range strings.Split(log, "\n") {
		sha, refs, _ := strings.Cut(line, "\x00")
		if i == 0 {
			head = sha
		}
		var names []string
		for n := range strings.SplitSeq(refs, ", ") {
			if n = strings.TrimPrefix(n, "HEAD -> "); n != "" && n != "HEAD" && n != current {
				names = append(names, n)
			}
		}
		if len(names) > 0 {
			slices.SortFunc(names, func(a, b string) int {
				if (a == def) != (b == def) {
					if a == def {
						return 1
					}
					return -1
				}
				return strings.Compare(a, b)
			})
			return names[0], head
		}
	}
	return "", head
}

// opOf says what git is in the middle of, from the checkout's own git folder: a worktree has its own, apart from the
// main checkout's .git.
func opOf(gitDir string) string {
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(gitDir, name))
		return err == nil
	}
	number := func(name string) int {
		b, _ := os.ReadFile(filepath.Join(gitDir, name))
		n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return n
	}
	for _, r := range [][3]string{{"rebase-merge", "msgnum", "end"}, {"rebase-apply", "next", "last"}} {
		if exists(r[0]) {
			if n, of := number(r[0]+"/"+r[1]), number(r[0]+"/"+r[2]); n > 0 && of > 0 {
				return "rebasing " + strconv.Itoa(n) + "/" + strconv.Itoa(of)
			}
			return "rebasing"
		}
	}
	switch {
	case exists("MERGE_HEAD"):
		return "merging"
	case exists("CHERRY_PICK_HEAD"):
		return "cherry-picking"
	case exists("REVERT_HEAD"):
		return "reverting"
	}
	return ""
}

// RepoOf names the repo a folder is in as Read names it, or "" outside git. A file names its folder's repo.
func RepoOf(dir string) string {
	if st, err := os.Stat(dir); err != nil {
		return ""
	} else if !st.IsDir() {
		dir = filepath.Dir(dir)
	}
	ctx, done := context.WithTimeout(context.Background(), 2*time.Second)
	defer done()
	if origin, err := git(ctx, dir, "config", "--get", "remote.origin.url"); err == nil && origin != "" {
		return repoName(origin, "")
	}
	top, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return ""
	}
	return repoName("", top)
}

// TopOf is the top folder of the checkout path is in, the nearest that holds .git, or "" outside git. It starts no
// git, so a hook can ask for every file an agent edits. A file names its folder's checkout, and a path that doesn't
// exist yet its parent's.
func TopOf(path string) string {
	if !filepath.IsAbs(path) {
		return ""
	}
	for dir := filepath.Clean(path); ; {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		up := filepath.Dir(dir)
		if up == dir {
			return ""
		}
		dir = up
	}
}

// Lockfiles change with every dependency bump and merge cleanly: two checkouts touching them is not an overlap.
var lockfiles = map[string]bool{
	"package-lock.json": true, "yarn.lock": true, "pnpm-lock.yaml": true, "bun.lock": true, "bun.lockb": true,
	"go.sum": true, "Cargo.lock": true, "uv.lock": true, "poetry.lock": true, "Pipfile.lock": true,
	"Gemfile.lock": true, "composer.lock": true, "flake.lock": true, "Podfile.lock": true, "pubspec.lock": true,
}

// unsharedAttrs are the .gitattributes that keep a file out of overlaps: generated or vendored code, binary
// files (-diff), and anything a repo marks murmur-ignore, such as data under traces/:
//
//	traces/** murmur-ignore
var unsharedAttrs = []string{"linguist-generated", "linguist-vendored", "murmur-ignore", "diff"}

// dropUnshared asks git which files those attributes cover, with the repo's own .gitattributes rules.
// When git fails, it keeps them all and says so.
func dropUnshared(ctx context.Context, top string, files []string) ([]string, error) {
	if len(files) == 0 {
		return files, nil
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", top, "-c", "core.fsmonitor=false", "check-attr", "-z", "--stdin"}, unsharedAttrs...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	cmd.Stdin = strings.NewReader(strings.Join(files, "\x00") + "\x00")
	out, err := cmd.Output()
	if err != nil {
		return files, err
	}
	skip := map[string]bool{}
	f := strings.Split(string(out), "\x00") // path, attribute, value, path, attribute, value...
	for i := 0; i+2 < len(f); i += 3 {
		path, attr, val := f[i], f[i+1], f[i+2]
		if (attr == "diff" && val == "unset") || (attr != "diff" && (val == "set" || val == "true")) {
			skip[path] = true
		}
	}
	return slices.DeleteFunc(files, func(p string) bool { return skip[p] }), nil
}
