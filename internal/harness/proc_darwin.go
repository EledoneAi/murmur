package harness

import (
	"strconv"

	"golang.org/x/sys/unix"
)

func kinfo(pid int) (*unix.KinfoProc, bool) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	return k, err == nil && int(k.Proc.P_pid) == pid
}

// parent is process pid's parent and its command's name.
func parent(pid int) (int, string, bool) {
	k, ok := kinfo(pid)
	if !ok {
		return 0, "", false
	}
	return int(k.Eproc.Ppid), unix.ByteSliceToString(k.Proc.P_comm[:]), true
}

func started(pid int) string {
	k, ok := kinfo(pid)
	if !ok {
		return ""
	}
	t := k.Proc.P_starttime
	return strconv.FormatInt(int64(t.Sec), 10) + "." + strconv.FormatInt(int64(t.Usec), 10)
}
