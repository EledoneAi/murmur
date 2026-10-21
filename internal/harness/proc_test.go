package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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

// A process's program, arguments and environment read as it was started: the environment only as far as keep lets
// it, and nothing for a process that is gone. The process is this test's binary: macOS hides the environment of its
// own programs (/bin/sh) from other processes.
func TestProcessStart(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "-test.run=^TestSleeper$", "")
	cmd.Env = []string{"MURMUR_TEST_SLEEPER=1", "KEEP_ME=1", "SECRET_THING=hush", "AWS_REGION=eu-west-1", "EMPTY="}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	if got, want := resolved(t, Executable(pid)), resolved(t, self); got != want {
		t.Errorf("executable %q, want %q", got, want)
	}
	if got := Args(pid); !slices.Equal(got, []string{self, "-test.run=^TestSleeper$", ""}) {
		t.Errorf("args %q", got)
	}
	keep := func(name string) bool { return name == "KEEP_ME" || name == "AWS_REGION" || name == "EMPTY" }
	if env, ok := Environ(pid, keep); !ok || !slices.Equal(env, []string{"KEEP_ME=1", "AWS_REGION=eu-west-1", "EMPTY="}) {
		t.Errorf("environment %q, %v", env, ok)
	}

	if got, want := resolved(t, Executable(os.Getpid())), resolved(t, self); got != want {
		t.Errorf("this test's executable %q, want %q", got, want)
	}
	if env, ok := Environ(os.Getpid(), func(name string) bool { return name == "HOME" }); !ok || !slices.Equal(env, []string{"HOME=" + os.Getenv("HOME")}) {
		t.Errorf("this test's environment %q, %v", env, ok)
	}

	gone := exec.Command("/usr/bin/true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{gone.Process.Pid, 0, -1} {
		if env, ok := Environ(pid, keep); ok || env != nil || Executable(pid) != "" || Args(pid) != nil {
			t.Errorf("pid %d, not running: %q %v, %q, %q", pid, env, ok, Executable(pid), Args(pid))
		}
	}
}

func resolved(t *testing.T, path string) string {
	t.Helper()
	if path == "" {
		return ""
	}
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestSleeper is the process TestProcessStart starts: it waits to be killed.
func TestSleeper(t *testing.T) {
	if os.Getenv("MURMUR_TEST_SLEEPER") != "1" {
		t.Skip("started by TestProcessStart")
	}
	time.Sleep(time.Minute)
}

// A process's start by the clock is the one ps prints, to the second; a process that is gone has none.
func TestStartedAt(t *testing.T) {
	cmd := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(os.Getpid()))
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		t.Skip("no ps here:", err)
	}
	want, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(strings.Fields(string(out)), " "), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if got := StartedAt(os.Getpid()); got.Truncate(time.Second).Sub(want).Abs() > time.Second {
		t.Fatalf("started at %s, ps says %s", got, want)
	}
	gone := exec.Command("/usr/bin/true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{gone.Process.Pid, 0, -1} {
		if got := StartedAt(pid); !got.IsZero() {
			t.Errorf("pid %d, not running, started at %s", pid, got)
		}
	}
}
