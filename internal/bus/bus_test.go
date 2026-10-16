package bus_test

import (
	"crypto/rand"
	"os"
	"strings"
	"testing"

	"github.com/EledoneAi/murmur/v2/internal/bus"
	"github.com/EledoneAi/murmur/v2/internal/bus/bustest"
)

func TestLocal(t *testing.T) {
	bustest.Run(t, func(t *testing.T) bus.Bus { return bus.NewLocal() })
}

// An unknown scheme names the valid ones, and never repeats the URL, which may hold a password.
func TestOpenUnknown(t *testing.T) {
	_, err := bus.Open(t.Context(), "postgress://hub:secret@db/hub")
	if err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "postgres://") {
		t.Errorf("Open: %v", err)
	}
}

// backend reads a test server's URL from env, or skips the test, and gives the test a channel of its own.
func backend(t *testing.T, env string) (url, channel string) {
	url = os.Getenv(env)
	if url == "" {
		t.Skipf("%s is not set", env)
	}
	channel = "bustest_" + rand.Text()
	if strings.Contains(url, "?") {
		return url + "&channel=" + channel, channel
	}
	return url + "?channel=" + channel, channel
}

func opener(url string) func(t *testing.T) bus.Bus {
	return func(t *testing.T) bus.Bus {
		t.Helper()
		b, err := bus.Open(t.Context(), url)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return b
	}
}
