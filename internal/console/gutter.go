package console

import (
	"cmp"
	"slices"
	"strings"
)

// The nerve gutter: seven cells left of a constellation's lamps, where the links between its agents run. Each link
// takes a lane, a vertical line between its two rows, and turns right at each end into the lamp. Bright lines join
// different people's agents, dim ones a person's own, and dashed ones carry a message a mute holds for you.

const (
	gutterW  = 7
	maxLanes = 3 // lanes at cells 0, 2 and 4; a crowded constellation draws the longest links first
)

// gutter returns gutterW cells for each member row, and for the gap under each row, where the detail line of a
// selected row goes and the links passing it carry on.
func (st styles) gutter(g glyphs, links []link, rows int) (out, gaps []string) {
	type laned struct {
		link
		lane int
	}
	byLen := slices.Clone(links)
	slices.SortStableFunc(byLen, func(x, y link) int { return cmp.Compare(y.b-y.a, x.b-x.a) }) // long links outside
	var placed []laned
	for _, l := range byLen {
		for lane := range maxLanes {
			free := !slices.ContainsFunc(placed, func(p laned) bool { return p.lane == lane && p.a <= l.b && l.a <= p.b })
			if free {
				placed = append(placed, laned{l, lane})
				break
			}
		}
	}
	out, gaps = make([]string, rows), make([]string, rows)
	for r := range rows {
		var gap strings.Builder
		for x := range gutterW {
			i := slices.IndexFunc(placed, func(p laned) bool { return x%2 == 0 && 2*p.lane == x && p.a <= r && r < p.b })
			if i < 0 {
				gap.WriteByte(' ')
				continue
			}
			v := g.v
			if placed[i].held {
				v = g.vDash
			}
			gap.WriteString(st.linkStyle(placed[i].link).Render(v))
		}
		gaps[r] = gap.String()
		var b strings.Builder
		for x := range gutterW {
			lane, onLane := x/2, x%2 == 0 && x/2 < maxLanes
			var own *laned // the link in this lane covering this row
			if onLane {
				for i := range placed {
					if p := &placed[i]; p.lane == lane && p.a <= r && r <= p.b {
						own = p
					}
				}
			}
			var from *laned // the nearest link to the left turning into its lamp on this row
			for i := range placed {
				if p := &placed[i]; (p.a == r || p.b == r) && 2*p.lane < x && (from == nil || p.lane > from.lane) {
					from = p
				}
			}
			left := from != nil
			switch {
			case own != nil:
				up, down := own.a < r, own.b > r
				right := own.a == r || own.b == r || left
				b.WriteString(st.linkStyle(own.link).Render(boxChar(g, up, down, left, right, own.held)))
			case left:
				h := g.h
				if from.held {
					h = g.hDash
				}
				b.WriteString(st.linkStyle(from.link).Render(h))
			default:
				b.WriteByte(' ')
			}
		}
		out[r] = b.String()
	}
	return out, gaps
}

func (st styles) linkStyle(l link) interface{ Render(...string) string } {
	switch {
	case l.held:
		return st.teal
	case l.between:
		return st.link
	}
	return st.sib
}

// boxChar is the line drawing for a lane cell that connects up, down, left and right.
func boxChar(g glyphs, up, down, left, right, dashed bool) string {
	switch {
	case up && down && left:
		return g.cross
	case up && down && right:
		return g.leftTee
	case up && down:
		if dashed {
			return g.vDash
		}
		return g.v
	case down && left:
		return g.tee
	case up && left:
		return g.bottomTee
	case down:
		return g.tl
	case up:
		return g.bl
	}
	if dashed {
		return g.hDash
	}
	return g.h
}
