package ui

// scrollWindow is the slice of table rows on screen: rows
// [offset, offset+count), plus whether a "N more" indicator line is shown
// above and/or below them.
type scrollWindow struct {
	offset, count        int
	showAbove, showBelow bool
}

// scrollTable picks the visible window over n rows for a row area of
// height lines, keeping cursor on screen with up to margin rows of context
// either side. prevOffset is the last window's offset: the window only
// moves when the cursor would otherwise get closer than margin to an edge,
// so moving within the window doesn't jitter it. Indicator lines are taken
// out of height, so the result never needs more than height lines.
// height <= 0 means unbounded (every row shown).
func scrollTable(n, height, cursor, prevOffset, margin int) scrollWindow {
	if n <= 0 {
		return scrollWindow{}
	}
	if height <= 0 || n <= height {
		return scrollWindow{offset: 0, count: n}
	}
	cursor = clampInt(cursor, 0, n-1)

	if height < 3 {
		// No room for indicators: plain window over the rows.
		off := clampInt(prevOffset, 0, n-height)
		if cursor < off {
			off = cursor
		}
		if cursor >= off+height {
			off = cursor - height + 1
		}
		return scrollWindow{offset: off, count: height}
	}

	// window reports how many rows fit at a given offset once the
	// indicator lines it implies are subtracted.
	window := func(off int) (count int, above, below bool) {
		above = off > 0
		count = height
		if above {
			count--
		}
		if off+count < n {
			below = true
			count--
		}
		return count, above, below
	}
	// The last offset still worth scrolling to: the bottom rows fill
	// everything under the "more above" line.
	maxOff := n - (height - 1)

	off := clampInt(prevOffset, 0, maxOff)
	for i := 0; i < 4; i++ {
		count, _, _ := window(off)
		mg := margin
		if limit := (count - 1) / 2; mg > limit {
			mg = limit
		}
		next := off
		if cursor < off+mg {
			next = cursor - mg
		} else if cursor > off+count-1-mg {
			next = cursor - (count - 1 - mg)
		}
		next = clampInt(next, 0, maxOff)
		if next == off {
			break
		}
		off = next
	}

	// Guarantee visibility even if the margin adjustment above didn't
	// settle (the indicator lines change the window size as it moves).
	for i := 0; i < n; i++ {
		count, _, _ := window(off)
		if cursor < off {
			off--
		} else if cursor >= off+count {
			off++
		} else {
			break
		}
	}
	count, above, below := window(off)
	return scrollWindow{offset: off, count: count, showAbove: above, showBelow: below}
}

func clampInt(v, lo, hi int) int {
	if hi < lo {
		hi = lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
