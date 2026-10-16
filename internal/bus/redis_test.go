package bus_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/EledoneAi/murmur/v2/internal/bus/bustest"
	"github.com/redis/go-redis/v9"
)

// BUS_TEST_REDIS=redis://localhost:6379
func TestRedis(t *testing.T) {
	url, _ := backend(t, "BUS_TEST_REDIS")
	bustest.RunShared(t, opener(url))
}

func TestRedisReconnect(t *testing.T) {
	url, channel := backend(t, "BUS_TEST_REDIS")
	// The client name tells the bus's subscribed connection apart from every other one on the server.
	b := opener(url + "&client_name=" + channel)(t)
	defer b.Close()
	opt, err := redis.ParseURL(os.Getenv("BUS_TEST_REDIS"))
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opt)
	defer rdb.Close()
	clients, err := rdb.ClientList(t.Context()).Result()
	if err != nil {
		t.Fatal(err)
	}
	var killed int64
	for line := range strings.Lines(clients) {
		fields := strings.Fields(line)
		if slices.Contains(fields, "name="+channel) && slices.Contains(fields, "sub=1") {
			n, err := rdb.ClientKillByFilter(t.Context(), "ID", strings.TrimPrefix(fields[0], "id=")).Result()
			if err != nil {
				t.Fatal(err)
			}
			killed += n
		}
	}
	if killed != 1 {
		t.Fatalf("killed %d subscribed connections, want 1", killed)
	}
	bustest.Recovers(t, b)
}
