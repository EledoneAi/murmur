package ipc

import (
	"bufio"
	"net"
	"os"
	"testing"

	"github.com/EledoneAi/murmur/internal/config"
)

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
