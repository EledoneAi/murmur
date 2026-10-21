package harness

import (
	"encoding/binary"
	"os"
	"slices"
	"testing"
)

// KERN_PROCARGS2 pads the program's path with NULs; an empty argument still counts; the environment ends at an empty
// string, whatever follows it.
func TestSplitArgs(t *testing.T) {
	b := binary.NativeEndian.AppendUint32(nil, 3)
	b = append(b, "/opt/homebrew/bin/claude\x00\x00\x00\x00claude\x00\x00--resume\x00A=1\x00B=\x00\x00\x00ptr_munge=\x00"...)
	exe, argv, env := splitArgs(b)
	if exe != "/opt/homebrew/bin/claude" || !slices.Equal(argv, []string{"claude", "", "--resume"}) || string(env) != "A=1\x00B=\x00" {
		t.Fatalf("exe %q argv %q env %q", exe, argv, env)
	}
	b = binary.NativeEndian.AppendUint32(nil, 2)
	if exe, argv, env := splitArgs(append(b, "/bin/x\x00x\x00y"...)); exe != "/bin/x" || !slices.Equal(argv, []string{"x", "y"}) || len(env) != 0 {
		t.Fatalf("cut short: exe %q argv %q env %q", exe, argv, env)
	}
	if exe, argv, env := splitArgs(binary.NativeEndian.AppendUint32(nil, 1)); exe != "" || argv != nil || env != nil {
		t.Fatalf("empty: exe %q argv %q env %q", exe, argv, env)
	}
}

// proc_pidpath reads the file a process runs, for one started with a relative path.
func TestPIDPath(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := resolved(t, pidPath(os.Getpid())), resolved(t, self); got != want {
		t.Fatalf("%q, want %q", got, want)
	}
	if got := pidPath(-1); got != "" {
		t.Fatalf("no process: %q", got)
	}
}
