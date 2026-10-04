package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"slices"
	"sync"
	"time"
)

// historyLockWait bounds how long an append waits for the history lock, so
// a stuck holder cannot stall a run past its deadline.
const historyLockWait = 5 * time.Second

// appendHistory appends rec as one line with a single write under the
// exclusive history lock. If the file does not end in a newline (a record
// torn by a crash), the line is prefixed with one so it starts fresh.
func (s *Store) appendHistory(rec attemptJSON) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("store: encoding attempt: %w", err)
	}
	line = append(line, '\n')
	lock, err := lockFile(s.lockPath, true, historyLockWait)
	if err != nil {
		return fmt.Errorf("store: history lock: %w", err)
	}
	defer lock.Close()
	f, err := os.OpenFile(s.historyPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], info.Size()-1); err == nil && last[0] != '\n' {
			line = append([]byte{'\n'}, line...)
		}
	}
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("store: appending history: %w", err)
	}
	return nil
}

// histCache holds parsed history and how far the file has been consumed.
type histCache struct {
	info     os.FileInfo // identity of the file read so far
	offset   int64       // bytes consumed (always just after a newline)
	attempts []Attempt
}

// refresh brings the cache up to date with the file. Caller holds s.mu.
func (s *Store) refreshHistory() error {
	f, err := os.Open(s.historyPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.hist = histCache{}
			return nil
		}
		return fmt.Errorf("store: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	h := &s.hist
	if h.info == nil || !os.SameFile(h.info, info) || info.Size() < h.offset {
		*h = histCache{info: info}
		if s.tailBytes > 0 && info.Size() > s.tailBytes {
			// Start inside the file: skip to just after the first newline
			// at or after the window start (the line in progress there is
			// partial).
			start := info.Size() - s.tailBytes
			skip, err := skipToLineStart(f, start-1)
			if err != nil {
				return err
			}
			h.offset = skip
		}
	}
	h.info = info
	if info.Size() == h.offset {
		return nil
	}
	buf := make([]byte, info.Size()-h.offset)
	n, err := f.ReadAt(buf, h.offset)
	if err != nil && err != io.EOF {
		return fmt.Errorf("store: reading history: %w", err)
	}
	buf = buf[:n]
	end := bytes.LastIndexByte(buf, '\n')
	if end < 0 {
		return nil // only a partial (or torn) line so far
	}
	h.attempts = append(h.attempts, parseLines(buf[:end])...)
	h.offset += int64(end + 1)
	return nil
}

// parallelParseMin is the read size above which lines are parsed by
// several goroutines (a cold load of a large history).
const parallelParseMin = 1 << 20

// parseLines decodes newline-separated attempt records in order, skipping
// empty, unparseable and foreign lines.
func parseLines(buf []byte) []Attempt {
	n := runtime.GOMAXPROCS(0)
	if len(buf) < parallelParseMin || n < 2 {
		return parseChunk(buf)
	}
	// Split at line boundaries into n chunks parsed concurrently.
	var chunks [][]byte
	for len(buf) > 0 {
		cut := min(len(buf), len(buf)/(n-len(chunks))+1)
		if i := bytes.IndexByte(buf[cut-1:], '\n'); i >= 0 {
			cut += i
		} else {
			cut = len(buf)
		}
		chunks = append(chunks, buf[:cut])
		buf = buf[cut:]
		if len(chunks) == n-1 && len(buf) > 0 {
			chunks = append(chunks, buf)
			break
		}
	}
	results := make([][]Attempt, len(chunks))
	var wg sync.WaitGroup
	for i, c := range chunks {
		wg.Go(func() { results[i] = parseChunk(c) })
	}
	wg.Wait()
	return slices.Concat(results...)
}

func parseChunk(buf []byte) []Attempt {
	var out []Attempt
	for line := range bytes.SplitSeq(buf, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var aj attemptJSON
		if json.Unmarshal(line, &aj) != nil || aj.Task == "" {
			continue // torn or foreign line
		}
		out = append(out, aj.Attempt)
	}
	return out
}

// skipToLineStart returns the offset just after the first newline at or
// after off (or the file size if there is none).
func skipToLineStart(f *os.File, off int64) (int64, error) {
	if off < 0 {
		return 0, nil
	}
	buf := make([]byte, 64<<10)
	for {
		n, err := f.ReadAt(buf, off)
		if i := bytes.IndexByte(buf[:n], '\n'); i >= 0 {
			return off + int64(i) + 1, nil
		}
		off += int64(n)
		if err == io.EOF {
			return off, nil
		}
		if err != nil {
			return 0, fmt.Errorf("store: reading history: %w", err)
		}
	}
}

// Query filters history. Zero fields match everything.
type Query struct {
	Task, Variant string
	Outcomes      []Outcome
	Since         time.Time // attempts whose end (else start, else queue) time is not before Since
	Worktree      string
	Limit         int // keep only the newest Limit matches
}

func (q Query) match(a *Attempt) bool {
	return (q.Task == "" || a.Task == q.Task) &&
		(q.Variant == "" || a.Variant == q.Variant) &&
		(q.Worktree == "" || a.Worktree == q.Worktree) &&
		(len(q.Outcomes) == 0 || slices.Contains(q.Outcomes, a.Outcome)) &&
		(q.Since.IsZero() || !a.when().Before(q.Since))
}

// History returns matching attempts from the history index in file order
// (newest last). A truncated final line is skipped, as are unparseable lines.
func (s *Store) History(q Query) ([]Attempt, error) {
	var out []Attempt
	err := s.scanHistory(func(all []Attempt) {
		for i := range all {
			if q.match(&all[i]) {
				out = append(out, all[i])
			}
		}
	})
	if err != nil {
		return nil, err
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[len(out)-q.Limit:]
	}
	return out, nil
}

// scanHistory refreshes the cache and calls fn with it under the lock. fn
// must not retain or modify the slice.
func (s *Store) scanHistory(fn func([]Attempt)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshHistory(); err != nil {
		return err
	}
	fn(s.hist.attempts)
	return nil
}
