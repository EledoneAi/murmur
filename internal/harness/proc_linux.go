package harness

import (
	"bytes"
	"os"
	"strconv"
)

// parent is process pid's parent and its command's name, from /proc/<pid>/stat: "pid (comm) state ppid …". The name
// may hold spaces and parentheses, so it ends at the last ")".
func parent(pid int) (int, string, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, "", false
	}
	open, end := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return 0, "", false
	}
	f := bytes.Fields(b[end+1:])
	if len(f) < 2 {
		return 0, "", false
	}
	ppid, err := strconv.Atoi(string(f[1]))
	return ppid, string(b[open+1 : end]), err == nil
}
