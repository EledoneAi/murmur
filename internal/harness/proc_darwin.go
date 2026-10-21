package harness

import (
	"bytes"
	"encoding/binary"
	"strconv"
	"syscall"
	"time"
	"unsafe"

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

func startedAt(pid int) time.Time {
	k, ok := kinfo(pid)
	if !ok {
		return time.Time{}
	}
	t := k.Proc.P_starttime
	return time.Unix(int64(t.Sec), int64(t.Usec)*1000)
}

func started(pid int) string {
	k, ok := kinfo(pid)
	if !ok {
		return ""
	}
	t := k.Proc.P_starttime
	return strconv.FormatInt(int64(t.Sec), 10) + "." + strconv.FormatInt(int64(t.Usec), 10)
}

// procArgs reads KERN_PROCARGS2: argc (32 bits, in the machine's order), the path the program was started with, NULs,
// then argc arguments and the environment, each ending with a NUL. An empty string ends the environment; what follows
// is the system's own. The system hands it only to the process's own user.
func procArgs(pid int) []byte {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(b) < 4 {
		return nil
	}
	return b
}

// splitArgs splits what procArgs read into the path the program was started with, its arguments and its environment.
func splitArgs(b []byte) (exe string, argv []string, env []byte) {
	argc := int(binary.NativeEndian.Uint32(b))
	b = b[4:]
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return "", nil, nil
	}
	exe, b = string(b[:i]), bytes.TrimLeft(b[i:], "\x00")
	for ; argc > 0 && len(b) > 0; argc-- {
		arg, rest, _ := bytes.Cut(b, []byte{0})
		argv, b = append(argv, string(arg)), rest
	}
	end := 0
	for end < len(b) && b[end] != 0 {
		v, _, found := bytes.Cut(b[end:], []byte{0})
		if end += len(v); found {
			end++
		}
	}
	return exe, argv, b[:end]
}

// executables are the path process pid was started with, when it is absolute, then the file it runs.
func executables(pid int) []string {
	var out []string
	if b := procArgs(pid); b != nil {
		exe, _, _ := splitArgs(b)
		clear(b)
		out = append(out, exe)
	}
	return append(out, pidPath(pid))
}

// pidPath is libproc's proc_pidpath: the file process pid runs, every link resolved. x/sys has no wrapper for it, so
// this makes the system call libproc makes, proc_info (336 since Mac OS X 10.5), which x/sys marks as deprecated with
// every other direct call.
func pidPath(pid int) string {
	const sysProcInfo, callPIDInfo, pidPathInfo, maxPath = 336, 2, 11, 4096
	buf := make([]byte, maxPath)
	_, _, errno := syscall.Syscall6(sysProcInfo, callPIDInfo, uintptr(pid), pidPathInfo, 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if errno != 0 {
		return ""
	}
	return unix.ByteSliceToString(buf)
}

func args(pid int) []string {
	b := procArgs(pid)
	if b == nil {
		return nil
	}
	defer clear(b)
	_, argv, _ := splitArgs(b)
	return argv
}

func environ(pid int, f func(env []byte)) bool {
	b := procArgs(pid)
	if b == nil {
		return false
	}
	defer clear(b)
	_, _, env := splitArgs(b)
	f(env)
	return true
}
