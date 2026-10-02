package term

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"
)

// AsciicastInfo reports lossy conversions made by WriteAsciicast.
type AsciicastInfo struct {
	Events int
	// InvalidBytes counts bytes that were not valid UTF-8 and were replaced
	// by U+FFFD. Asciicast output events are JSON strings and cannot carry
	// arbitrary bytes; the raw record stays authoritative.
	InvalidBytes int
}

// WriteAsciicast exports the capture as asciicast v2 (a derived replay
// format). All selected streams are merged in read order as "o" events;
// resizes become "r" events. A UTF-8 sequence split across chunks is carried
// into the next event; invalid bytes become U+FFFD.
func (c *Capture) WriteAsciicast(w io.Writer, streams []string) (AsciicastInfo, error) {
	var info AsciicastInfo
	bw := bufio.NewWriter(w)
	hdr := map[string]any{"version": 2, "width": c.Start.Cols, "height": c.Start.Rows}
	if t, err := time.Parse(time.RFC3339Nano, c.Start.Wall); err == nil {
		hdr["timestamp"] = t.Unix()
	}
	if c.Start.Term != "" {
		hdr["env"] = map[string]string{"TERM": c.Start.Term}
	}
	b, _ := json.Marshal(hdr)
	bw.Write(append(b, '\n'))
	var carry []byte
	lnm := false
	emit := func(ns int64, code, s string) {
		b, _ := json.Marshal([]any{float64(ns) / 1e9, code, s})
		bw.Write(append(b, '\n'))
		info.Events++
	}
	err := c.Replay(func(ev Event, data []byte) error {
		switch ev.T {
		case "resize":
			emit(ev.NS, "r", fmt.Sprintf("%dx%d", ev.Cols, ev.Rows))
		case "chunk":
			if !wanted(streams, ev.S) {
				return nil
			}
			if !lnm && !c.ttyStream(ev.S) {
				// Pipe output has bare LF; asciinema players need CRLF.
				lnm = true
				emit(ev.NS, "o", "\x1b[20h")
			}
			p := append(carry, data...)
			carry = nil
			// Hold back an incomplete trailing sequence.
			cut := len(p)
			for i := len(p) - 1; i >= 0 && i >= len(p)-utf8.UTFMax; i-- {
				if utf8.RuneStart(p[i]) {
					if !utf8.FullRune(p[i:]) {
						cut = i
					}
					break
				}
			}
			carry = append([]byte(nil), p[cut:]...)
			s, bad := toValidUTF8(p[:cut])
			info.InvalidBytes += bad
			if s != "" {
				emit(ev.NS, "o", s)
			}
		}
		return nil
	})
	if len(carry) > 0 {
		s, bad := toValidUTF8(carry)
		info.InvalidBytes += bad
		emit(0, "o", s)
	}
	if errors.Is(err, ErrTruncatedJournal) {
		err = nil
	}
	if err != nil {
		return info, err
	}
	return info, bw.Flush()
}

func toValidUTF8(p []byte) (string, int) {
	if utf8.Valid(p) {
		return string(p), 0
	}
	bad := 0
	out := make([]rune, 0, len(p))
	for len(p) > 0 {
		r, n := utf8.DecodeRune(p)
		if r == utf8.RuneError && n == 1 {
			bad++
		}
		out = append(out, r)
		p = p[n:]
	}
	return string(out), bad
}
