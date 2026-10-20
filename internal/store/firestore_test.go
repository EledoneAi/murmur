//go:build !laptop

package store

import (
	"crypto/rand"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFirestorePruneEvents(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("FIRESTORE_EMULATOR_HOST is not set")
	}
	ctx := t.Context()
	st, err := openFirestore(ctx, "test-"+strings.ToLower(rand.Text()[:12]), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := st.(*fireStore)

	now := time.Now().Truncate(time.Microsecond)
	recent := Event{At: now.Add(-time.Hour), Kind: "sign-in"}
	old := Event{At: now.Add(-keepEventsFor - time.Hour), Kind: "sign-in"}
	older := Event{At: now.Add(-2 * keepEventsFor), Kind: "sign-in"}
	for _, e := range []Event{recent, old, older} {
		if err := s.AddEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.pruneEvents(ctx, now); err != nil {
		t.Fatal(err)
	}
	left, err := s.Events(ctx, 10)
	if err != nil || len(left) != 1 || !left[0].At.Equal(recent.At) {
		t.Errorf("events left: %v, %v; want the recent one", left, err)
	}
}
