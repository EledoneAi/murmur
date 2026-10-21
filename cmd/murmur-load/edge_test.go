package main

import (
	"flag"
	"testing"
)

// Flags the load would panic on later, once laptops run, are refused before anything starts.
func TestBadFlags(t *testing.T) {
	set := func(kv ...string) {
		for i := 0; i < len(kv); i += 2 {
			if err := flag.Set(kv[i], kv[i+1]); err != nil {
				t.Fatal(err)
			}
		}
	}
	defaults := func() {
		for _, f := range []string{"laptops", "agents", "repos", "send", "every"} {
			set(f, flag.Lookup(f).DefValue)
		}
	}
	defer defaults()
	for _, bad := range [][]string{{"laptops", "0"}, {"laptops", "-1"}, {"laptops", "1"}, {"agents", "0"}, {"repos", "0"},
		{"every", "0s"}, {"every", "-1s"}} {
		defaults()
		set(bad...)
		if badFlags() == nil {
			t.Errorf("-%s %s: accepted", bad[0], bad[1])
		}
	}
	defaults()
	if err := badFlags(); err != nil {
		t.Errorf("the defaults: %v", err)
	}
	set("laptops", "1", "send", "0s")
	if err := badFlags(); err != nil {
		t.Errorf("one laptop that sends nothing: %v", err)
	}
}
