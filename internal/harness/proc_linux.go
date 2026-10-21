package harness

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"time"
)

// stat reads /proc/<pid>/stat: "pid (comm) state ppid … starttime …". The name may hold spaces and parentheses, so it
// ends at the last ")"; the fields after it start with the state, the third field.
func stat(pid int) (name string, fields [][]byte, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", nil, false
	}
	open, end := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return "", nil, false
	}
	return string(b[open+1 : end]), bytes.Fields(b[end+1:]), true
}

// parent is process pid's parent and its command's name.
func parent(pid int) (int, string, bool) {
	name, f, ok := stat(pid)
	if !ok || len(f) < 2 {
		return 0, "", false
	}
	ppid, err := strconv.Atoi(string(f[1]))
	return ppid, name, err == nil
}

// started is the process's start time in clock ticks after boot, the 22nd field.
func started(pid int) string {
	_, f, ok := stat(pid)
	if !ok || len(f) < 20 {
		return ""
	}
	return string(f[19])
}

// startedAt is the system's boot time, from /proc/stat, plus the process's start time in clock ticks: the kernel
// shows them in USER_HZ, 100 a second.
func startedAt(pid int) time.Time {
	ticks, err := strconv.ParseInt(started(pid), 10, 64)
	if err != nil {
		return time.Time{}
	}
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for line := range strings.Lines(string(b)) {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			if boot, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return time.Unix(boot, 0).Add(time.Duration(ticks) * time.Second / 100)
			}
		}
	}
	return time.Time{}
}

func procFile(pid int, name string) string { return "/proc/" + strconv.Itoa(pid) + "/" + name }

// executables is the file process pid runs. Linux keeps no path it was started with beyond argv[0], which a shell
// sets to the command's name. A file replaced since the process started reads as "path (deleted)": the path may hold
// its new version.
func executables(pid int) []string {
	p, err := os.Readlink(procFile(pid, "exe"))
	if err != nil {
		return nil
	}
	return []string{strings.TrimSuffix(p, " (deleted)")}
}

func args(pid int) []string {
	b, err := os.ReadFile(procFile(pid, "cmdline"))
	if err != nil || len(b) == 0 {
		return nil
	}
	defer clear(b)
	var argv []string
	for v := range bytes.SplitSeq(bytes.TrimSuffix(b, []byte{0}), []byte{0}) {
		argv = append(argv, string(v))
	}
	return argv
}

func environ(pid int, f func(env []byte)) bool {
	b, err := os.ReadFile(procFile(pid, "environ"))
	if err != nil {
		return false
	}
	defer clear(b)
	f(b)
	return true
}
