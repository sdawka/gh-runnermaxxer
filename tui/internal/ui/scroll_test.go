package ui

import "testing"

// TestScrollTableKeepsCursorVisible brute-forces every row count, height,
// cursor and previous offset in a small range: the window must always
// contain the cursor, fit in height lines (indicators included), and only
// show an indicator when rows are actually hidden on that side.
func TestScrollTableKeepsCursorVisible(t *testing.T) {
	for n := 1; n <= 20; n++ {
		for height := 1; height <= 12; height++ {
			for cursor := 0; cursor < n; cursor++ {
				for prev := 0; prev < n; prev++ {
					w := scrollTable(n, height, cursor, prev, scrollMargin)
					if cursor < w.offset || cursor >= w.offset+w.count {
						t.Fatalf("n=%d h=%d cursor=%d prev=%d: cursor outside window %+v", n, height, cursor, prev, w)
					}
					lines := w.count
					if w.showAbove {
						lines++
					}
					if w.showBelow {
						lines++
					}
					if lines > height {
						t.Fatalf("n=%d h=%d cursor=%d prev=%d: window needs %d lines: %+v", n, height, cursor, prev, lines, w)
					}
					if w.showAbove != (w.offset > 0) && height >= 3 {
						t.Fatalf("n=%d h=%d: showAbove=%v with offset %d", n, height, w.showAbove, w.offset)
					}
					if w.showBelow != (w.offset+w.count < n) && height >= 3 {
						t.Fatalf("n=%d h=%d: showBelow=%v with window end %d of %d", n, height, w.showBelow, w.offset+w.count, n)
					}
					if n <= height && (w.offset != 0 || w.count != n) {
						t.Fatalf("n=%d h=%d: everything fits but window is %+v", n, height, w)
					}
				}
			}
		}
	}
}

// TestScrollTableMargin: moving the cursor down one row at a time keeps
// scrollMargin rows of context below it until the end of the list, and the
// window doesn't move while the cursor is comfortably inside it.
func TestScrollTableMargin(t *testing.T) {
	n, height := 30, 10
	off := 0
	for cursor := 0; cursor < n; cursor++ {
		w := scrollTable(n, height, cursor, off, scrollMargin)
		end := w.offset + w.count
		if cursor+scrollMargin < n && end-1-cursor < scrollMargin {
			t.Fatalf("cursor %d: only %d rows below it in window %+v", cursor, end-1-cursor, w)
		}
		if cursor == 1 && w.offset != 0 {
			t.Fatalf("window scrolled with the cursor near the top: %+v", w)
		}
		off = w.offset
	}
}
