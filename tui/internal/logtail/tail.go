// Package logtail follows a runner log file the way `tail -f` would, with
// one addition runnermaxxer.sh needs: its log rotation (research §1.4)
// truncates the file *in place* rather than renaming it, so a plain
// growing-offset tail would get stuck reading garbage past the new EOF.
// Tail detects a shrinking file size and re-reads from the start.
package logtail

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"strings"
	"time"
)

// PollInterval is how often Tail checks the file for new data.
const PollInterval = 250 * time.Millisecond

// Update is one batch of new lines (or a reset after truncation) delivered
// on the channel Tail returns.
type Update struct {
	Lines []string
	Reset bool // true when the file shrank (rotation) and Lines replaces the buffer
	Err   error
}

// Tail returns the last n lines already in the file plus a channel that
// delivers further updates until ctx is cancelled (the channel is then
// closed). A missing file is not an error: Tail treats it as empty and
// keeps polling in case it appears (a runner that hasn't logged yet).
func Tail(ctx context.Context, path string, n int) ([]string, <-chan Update, error) {
	initial, offset, err := readTail(path, n)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}

	updates := make(chan Update, 8)
	go run(ctx, path, offset, updates)
	return initial, updates, nil
}

// readTail reads the whole file (log files are rotated below
// MAX_LOG_SIZE_MB, so this is bounded) and returns its last n lines plus
// the file's current size (the starting offset for the follower).
func readTail(path string, n int) ([]string, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	lines := splitLines(data)
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, int64(len(data)), nil
}

func splitLines(data []byte) []string {
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func run(ctx context.Context, path string, offset int64, updates chan<- Update) {
	defer close(updates)
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()

	var leftover []byte // a line fragment read before its trailing newline arrived

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			newOffset, newLeftover, upd, ok := poll(path, offset, leftover)
			if !ok {
				continue
			}
			offset = newOffset
			leftover = newLeftover
			if len(upd.Lines) == 0 && upd.Err == nil {
				continue
			}
			select {
			case updates <- upd:
			case <-ctx.Done():
				return
			}
		}
	}
}

// poll checks the file once. ok is false when there is nothing worth
// reporting (no change, or the file still doesn't exist). leftover carries
// an incomplete final line (no trailing newline yet) across polls.
func poll(path string, offset int64, leftover []byte) (newOffset int64, newLeftover []byte, upd Update, ok bool) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return offset, leftover, Update{}, false
		}
		return offset, leftover, Update{Err: err}, true
	}

	size := info.Size()
	if size == offset {
		return offset, leftover, Update{}, false
	}

	if size < offset {
		// Truncated in place (log rotation): re-read from the start.
		data, err := os.ReadFile(path)
		if err != nil {
			return offset, leftover, Update{Err: err}, true
		}
		lines := splitLines(data)
		return int64(len(data)), nil, Update{Lines: lines, Reset: true}, true
	}

	f, err := os.Open(path)
	if err != nil {
		return offset, leftover, Update{Err: err}, true
	}
	defer f.Close()

	if _, err := f.Seek(offset, 0); err != nil {
		return offset, leftover, Update{Err: err}, true
	}

	buf := append(append([]byte{}, leftover...), mustReadAll(f)...)
	lastNL := bytes.LastIndexByte(buf, '\n')
	if lastNL < 0 {
		// No complete line yet; keep buffering (report progress, no lines).
		return size, buf, Update{}, false
	}

	complete := buf[:lastNL]
	rest := append([]byte{}, buf[lastNL+1:]...)
	var lines []string
	for _, l := range bytes.Split(complete, []byte("\n")) {
		lines = append(lines, string(l))
	}
	return size, rest, Update{Lines: lines}, true
}

func mustReadAll(f *os.File) []byte {
	r := bufio.NewReader(f)
	var out bytes.Buffer
	_, _ = out.ReadFrom(r)
	return out.Bytes()
}
