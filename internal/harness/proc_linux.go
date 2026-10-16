package harness

import (
	"bytes"
	"os"
	"strconv"
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
