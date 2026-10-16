package ipc

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	"github.com/EledoneAi/murmur/internal/config"
	"github.com/EledoneAi/murmur/internal/release"
)

// A daemon older than this binary, left running by an upgrade it didn't make, is stopped and this version's starts;
// one of the same version or newer is left alone.
func TestEnsureDaemonReplacesAnOlderOne(t *testing.T) {
	dir, err := os.MkdirTemp("", "mm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("MURMUR_HOME", dir)
	defer func(v string, s func(string) error) { release.Version, start = v, s }(release.Version, start)
	release.Version = "1.1.0"
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
	for _, v := range []string{"1.1.0", "1.2.0"} {
		stopped := daemon(v)
		if err := EnsureDaemon(); err != nil || started != 0 || len(stopped) != 0 {
			t.Fatalf("a daemon of %s: %v, started %d", v, err, started)
		}
		_ = CallRunning(Request{Op: "stop"}, &Reply{}, time.Second)
		<-stopped
	}
	stopped := daemon("1.0.1")
	if err := EnsureDaemon(); err != nil || started != 1 || len(stopped) != 1 {
		t.Fatalf("a daemon of 1.0.1: %v, started %d, stopped %d", err, started, len(stopped))
	}
}

func TestSettledWaitsForTheHub(t *testing.T) {
	dir, err := os.MkdirTemp("", "mm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("MURMUR_HOME", dir)
	ln, err := net.Listen("unix", config.SockPath()) // a daemon that needs three status calls to come online
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for n := 0; ; n++ {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = bufio.NewReader(c).ReadString('\n')
			state := "connecting"
			if n >= 3 {
				state = "online"
			}
			_, _ = c.Write([]byte(`{"ok":true,"state":"` + state + `"}` + "\n"))
			c.Close()
		}
	}()
	if r, err := Settled(); err != nil || r.State != "online" {
		t.Fatalf("settled: %q, %v", r.State, err)
	}
}

// The daemon says "online" once the team list has arrived, not when the connection opens.
