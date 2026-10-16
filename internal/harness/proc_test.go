package harness

import (
	"os"
	"os/exec"
	"testing"
)

// A hook a shell runs finds the process above the shell; one the harness runs directly finds its own parent.
func TestProcess(t *testing.T) {
	if got := Process(os.Getpid()); got != os.Getpid() {
		t.Fatalf("a process that isn't a shell: %d, want itself, %d", got, os.Getpid())
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 5; true") // two commands: the shell stays
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	if _, name, ok := parent(cmd.Process.Pid); !ok || name != "sh" {
		t.Fatalf("the shell reads as %q, %v", name, ok)
	}
	if got := Process(cmd.Process.Pid); got != os.Getpid() {
		t.Fatalf("under a shell: %d, want this test, %d", got, os.Getpid())
	}
}
