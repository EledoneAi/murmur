package ipc

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/v2/internal/config"
	"github.com/EledoneAi/murmur/v2/internal/release"
)

// A daemon running a pre-release is older than its release: upgrading from 1.3.0-rc.1 to 1.3.0 replaces it, and a
// pre-release binary leaves its release's daemon alone.
func TestEnsureDaemonReplacesAPreRelease(t *testing.T) {
	dir, err := os.MkdirTemp("", "mm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("MURMUR_HOME", dir)
	defer func(v string, s func(string) error) { release.Version, start, replaced = v, s, false }(release.Version, start)
	started := 0
	start = func(string) error { started++; return nil }
	daemon := func(version string) (stopped chan bool) {
		ln, err := net.Listen("unix", config.SockPath())
		if err != nil {
			t.Fatal(err)
		}
		stopped = make(chan bool, 1)
		go func() {
			defer ln.Close()
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				line, _ := bufio.NewReader(c).ReadString('\n')
				var req Request
				_ = json.Unmarshal([]byte(line), &req)
				_ = json.NewEncoder(c).Encode(Reply{OK: true, Version: version})
				c.Close()
				if req.Op == "stop" {
					ln.Close() // as a daemon lets go of its socket when it stops
					stopped <- true
					return
				}
			}
		}()
		return stopped
	}
	release.Version = "1.3.0-rc.1"
	stopped := daemon("1.3.0")
	if err := EnsureDaemon(); err != nil || started != 0 || len(stopped) != 0 {
		t.Fatalf("a 1.3.0 daemon, from 1.3.0-rc.1: %v, started %d", err, started)
	}
	_ = CallRunning(Request{Op: "stop"}, &Reply{}, time.Second)
	<-stopped
	release.Version = "1.3.0"
	stopped = daemon("1.3.0-rc.1")
	if err := EnsureDaemon(); err != nil || started != 1 {
		t.Fatalf("a 1.3.0-rc.1 daemon, from 1.3.0: %v, started %d", err, started)
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the daemon of 1.3.0-rc.1 was never stopped")
	}
}
