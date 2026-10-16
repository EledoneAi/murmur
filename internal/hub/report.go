package hub

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

const reportKey = "chat/report" // the last week the report was posted for

// weeklyReport posts the previous week to the team's chat once, from Monday at the start of the working day. A chat
// that fails misses that week's report, rather than getting it again every jobEvery.
func (h *Hub) weeklyReport(ctx context.Context, now time.Time) {
	s := h.Settings()
	if !s.WeeklyReport || s.ChatWebhook == "" || !s.working(now) {
		return
	}
	t := now.In(zone(s.TimeZone))
	monday := time.Date(t.Year(), t.Month(), t.Day()-(int(t.Weekday())+6)%7, s.WorkStart, 0, 0, 0, t.Location())
	if t.Before(monday) {
		return
	}
	last := monday.AddDate(0, 0, -7)
	week, posted := weekOf(last, s.TimeZone), ""
	if err := getJSON(ctx, h.store, reportKey, &posted); err != nil {
		h.log.Error("database", "op", "read the last weekly report", "error", err.Error())
		return
	}
	if posted == week {
		return
	}
	total, err := h.weekTotals(ctx, week)
	if err != nil {
		h.log.Error("database", "op", "read the week's counts", "week", week, "error", err.Error())
		return
	}
	if err := h.store.Put(ctx, reportKey, mustJSON(week)); err != nil {
		h.log.Error("database", "op", "remember the weekly report", "week", week, "error", err.Error())
		return
	}
	text := reportText(last, total, h.cfg.PublicURL)
	if text == "" {
		return // a quiet week, or a new hub
	}
	h.log.Info("weekly report", "week", week)
	_ = h.post(ctx, text)
}

// reportText is the team's week in a few lines, or "" when there is nothing to say. It is about how the team's agents
// work together, so it gives team totals and never names anyone.
func reportText(monday time.Time, w weekStats, url string) string {
	var lines []string
	if n := total(w.Waits); n > 0 {
		lines = append(lines, fmt.Sprintf("- Agents waited for their people %s: the median wait was %s, the longest %s.",
			times(n), minutes(median(w.Waits)), minutes(slices.Max(slices.Collect(maps.Keys(w.Waits))))))
	}
	if len(w.Collisions) > 0 {
		files := slices.SortedFunc(maps.Keys(w.Collisions), func(a, b string) int {
			return cmp.Or(w.Collisions[b]-w.Collisions[a], strings.Compare(a, b))
		})
		var most []string
		for _, k := range files[:min(len(files), 5)] {
			repo, file, _ := strings.Cut(k, " ")
			most = append(most, fmt.Sprintf("%s in %s (%s)", file, repo, times(w.Collisions[k])))
		}
		lines = append(lines, "- Files two checkouts changed at once most often: "+strings.Join(most, ", ")+".")
	}
	if w.Crossed > 0 {
		lines = append(lines, fmt.Sprintf("- %s went between different people's agents.", plural(w.Crossed, "message", "messages")))
	}
	if w.Merges > 0 {
		lines = append(lines, fmt.Sprintf("- %s told agents a pull request had merged over their work.", plural(w.Merges, "merge notice", "merge notices")))
	}
	if len(lines) == 0 {
		return ""
	}
	text := "The team's week on murmur, from Monday " + monday.Format("2 January") + ":\n" + strings.Join(lines, "\n")
	if url != "" {
		text += "\n" + url
	}
	return text
}

// total counts the waits in a histogram of minutes.
func total(waits map[int]int) (n int) {
	for _, c := range waits {
		n += c
	}
	return n
}

// median is the middle wait, in minutes.
func median(waits map[int]int) int {
	half := (total(waits) + 1) / 2
	for _, m := range slices.Sorted(maps.Keys(waits)) {
		if half -= waits[m]; half <= 0 {
			return m
		}
	}
	return 0
}

func times(n int) string {
	if n == 1 {
		return "once"
	}
	return fmt.Sprintf("%d times", n)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
