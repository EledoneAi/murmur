package harness

import "golang.org/x/sys/unix"

// parent is process pid's parent and its command's name.
func parent(pid int) (int, string, bool) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(k.Proc.P_pid) != pid {
		return 0, "", false
	}
	return int(k.Eproc.Ppid), unix.ByteSliceToString(k.Proc.P_comm[:]), true
}
