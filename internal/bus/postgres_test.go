package bus_test

import (
	"context"
	"os"
	"testing"

	"github.com/EledoneAi/murmur/v2/internal/bus/bustest"
	"github.com/jackc/pgx/v5"
)

// BUS_TEST_POSTGRES=postgres://postgres:x@localhost:5432/postgres?sslmode=disable
func TestPostgres(t *testing.T) {
	url, _ := backend(t, "BUS_TEST_POSTGRES")
	bustest.RunShared(t, opener(url))
}

func TestPostgresReconnect(t *testing.T) {
	url, channel := backend(t, "BUS_TEST_POSTGRES")
	b := opener(url)(t)
	defer b.Close()
	db, err := pgx.Connect(t.Context(), os.Getenv("BUS_TEST_POSTGRES"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	// pg_terminate_backend waits for the connection to end, so nothing more reaches it once it returns.
	var ended int
	err = db.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE pg_terminate_backend(pid, 5000))
		FROM pg_stat_activity WHERE query = $1`, `LISTEN "`+channel+`"`).Scan(&ended)
	if err != nil || ended != 1 {
		t.Fatalf("ended %d listening connections (%v), want 1", ended, err)
	}
	bustest.Recovers(t, b)
}
