package hub

import (
	"errors"
	"testing"
)

func TestIgnored(t *testing.T) {
	s := Settings{IgnoredPaths: []string{"traces/**", "*.lock", "docs/*.md"}}
	for f, want := range map[string]bool{
		"traces/2026-09-16.jsonl": true, "traces/a/b.json": true, "src/traces/x": false, "yarn.lock": true, "web/app.lock": true,
		"docs/intro.md": true, "docs/guide/intro.md": false, "main.go": false,
	} {
		if s.Ignored(f) != want {
			t.Errorf("%s: ignored %v, want %v", f, !want, want)
		}
	}
	d := Defaults()
	if err := d.Valid(); err != nil {
		t.Fatal(err)
	}
	// every reason names the setting at fault, for the admin panel to point at it
	for field, bad := range map[string]func(*Settings){
		"update_policy": func(s *Settings) { s.UpdatePolicy = "sometimes" }, "min_version": func(s *Settings) { s.MinVersion = "one" },
		"ignored_paths": func(s *Settings) { s.IgnoredPaths = []string{"["} }, "sends_per_minute": func(s *Settings) { s.SendsPerMinute = 0 },
		"work_end": func(s *Settings) { s.WorkEnd = s.WorkStart }, "time_zone": func(s *Settings) { s.TimeZone = "Mars" },
		"allowed_emails":  func(s *Settings) { s.AllowedEmails = []string{"erin"} },
		"btw_per_hour":    func(s *Settings) { s.BtwPerHour = 1001 },
		"allowed_domains": func(s *Settings) { s.AllowedDomains = []string{"erin@x.com"} },
	} {
		s := Defaults()
		bad(&s)
		var e settingError
		if err := s.Valid(); !errors.As(err, &e) || e.field != field {
			t.Errorf("%s: %v", field, err)
		}
	}
}
