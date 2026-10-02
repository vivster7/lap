package term

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Event is one line of events.jsonl.
type Event struct {
	T  string `json:"t"`  // start, chunk, resize, input, eof, error, write_error, incomplete, end
	NS int64  `json:"ns"` // monotonic nanoseconds since start
	S  string `json:"s,omitempty"`
	// chunk: byte range in output.bytes. resize: logical offset at which it
	// applies. incomplete: logical offset of the missing range (N == -1:
	// unknown length, the stream's tail).
	Off  int64  `json:"off,omitempty"`
	N    int64  `json:"n,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	End  string `json:"end,omitempty"`
	Err  string `json:"err,omitempty"`
	// input: bytes written to the terminal (query replies).
	Data   string `json:"data,omitempty"`
	Reason string `json:"reason,omitempty"`

	// start only
	Version int      `json:"version,omitempty"`
	Mode    string   `json:"mode,omitempty"`
	Term    string   `json:"term,omitempty"`
	Streams []string `json:"streams,omitempty"`
	TTY     []bool   `json:"tty,omitempty"`
	Wall    string   `json:"wall,omitempty"`
	// Record is caller metadata from Spec.Record.
	Record map[string]string `json:"record,omitempty"`

	// end only
	Stored   int64 `json:"stored,omitempty"`
	Received int64 `json:"received,omitempty"`
	Complete bool  `json:"complete,omitempty"`
}

// Capture reads a stored capture. It works on finalized captures and on
// captures left behind by a crash (no capture.json, possibly a truncated last
// journal line, possibly unjournaled bytes at the end of output.bytes).
type Capture struct {
	Dir   string
	Start Event
	sum   *Summary
}

// OpenCapture opens the capture stored in dir.
func OpenCapture(dir string) (*Capture, error) {
	c := &Capture{Dir: dir}
	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("term: events.jsonl has no start record: %w", err)
	}
	if err := json.Unmarshal(line, &c.Start); err != nil || c.Start.T != "start" {
		return nil, fmt.Errorf("term: bad start record: %q", line)
	}
	if c.Start.Version != FormatVersion {
		return nil, fmt.Errorf("term: capture format %d not supported", c.Start.Version)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "capture.json")); err == nil {
		var s Summary
		if json.Unmarshal(b, &s) == nil {
			c.sum = &s
		}
	}
	return c, nil
}

// Summary returns the finalized summary, if the session reached Done.
// ok == false means the capture was never finalized (crash, or still
// running) and must be treated as incomplete.
func (c *Capture) Summary() (s Summary, ok bool) {
	if c.sum == nil {
		return Summary{}, false
	}
	return *c.sum, true
}

// Raw opens output.bytes: every stream's bytes in read order.
func (c *Capture) Raw() (*os.File, error) { return os.Open(filepath.Join(c.Dir, "output.bytes")) }

// ttyStream reports whether stream name was a terminal.
func (c *Capture) ttyStream(name string) bool {
	for i, s := range c.Start.Streams {
		if s == name && i < len(c.Start.TTY) {
			return c.Start.TTY[i]
		}
	}
	return false
}

// ErrTruncatedJournal reports a partial last line in events.jsonl.
var ErrTruncatedJournal = errors.New("term: events.jsonl ends with a partial record")

// StreamUnjournaled names bytes found in output.bytes after the last
// journaled chunk (a crash between the two writes).
const StreamUnjournaled = "unjournaled"

// Replay calls fn for every event in order. For chunk events data holds the
// chunk's bytes (valid only during the call). Memory is bounded by the
// largest chunk. A truncated final journal line is skipped and reported by
// returning ErrTruncatedJournal after all complete events; bytes after the
// last journaled chunk are delivered as a chunk of stream "unjournaled".
func (c *Capture) Replay(fn func(ev Event, data []byte) error) error {
	ef, err := os.Open(filepath.Join(c.Dir, "events.jsonl"))
	if err != nil {
		return err
	}
	defer ef.Close()
	of, err := c.Raw()
	if err != nil {
		return err
	}
	defer of.Close()
	or := bufio.NewReaderSize(of, 256<<10)
	var pos int64
	var buf []byte
	er := bufio.NewReaderSize(ef, 64<<10)
	truncated := false
	for {
		line, err := er.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] != '\n' {
			truncated = true
			break
		}
		if len(line) > 0 {
			var ev Event
			if jerr := json.Unmarshal(bytes.TrimSpace(line), &ev); jerr != nil {
				return fmt.Errorf("term: bad journal line %q: %w", line, jerr)
			}
			var data []byte
			if ev.T == "chunk" {
				if ev.Off != pos {
					return fmt.Errorf("term: chunk at %d, expected %d", ev.Off, pos)
				}
				if int64(cap(buf)) < ev.N {
					buf = make([]byte, ev.N)
				}
				data = buf[:ev.N]
				if _, rerr := io.ReadFull(or, data); rerr != nil {
					return fmt.Errorf("term: output.bytes shorter than journal: %w", rerr)
				}
				pos += ev.N
			}
			if ferr := fn(ev, data); ferr != nil {
				return ferr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	// Unjournaled tail.
	if buf == nil {
		buf = make([]byte, 32<<10)
	}
	for {
		n, rerr := or.Read(buf[:cap(buf)])
		if n > 0 {
			ev := Event{T: "chunk", S: StreamUnjournaled, Off: pos, N: int64(n)}
			pos += int64(n)
			if ferr := fn(ev, buf[:n]); ferr != nil {
				return ferr
			}
		}
		if rerr != nil {
			break
		}
	}
	if truncated {
		return ErrTruncatedJournal
	}
	return nil
}

// WriteStream writes the bytes of one stream (in order) to w.
func (c *Capture) WriteStream(w io.Writer, stream string) error {
	return ignoreTrunc(c.Replay(func(ev Event, data []byte) error {
		if ev.T == "chunk" && ev.S == stream {
			_, err := w.Write(data)
			return err
		}
		return nil
	}))
}

func ignoreTrunc(err error) error {
	if errors.Is(err, ErrTruncatedJournal) {
		return nil
	}
	return err
}

// PlainOptions selects streams for the plain projection.
type PlainOptions struct {
	// Streams to include; nil means all.
	Streams []string
	// Prefix prepends "<stream>| " to every line (useful for pipe mode).
	Prefix bool
}

// Plain writes the chronological plain-text projection (see
// PlainProjector) to w. Each stream has its own line state; committed lines
// are written in commit order.
func (c *Capture) Plain(w io.Writer, opt PlainOptions) error {
	bw := bufio.NewWriterSize(w, 64<<10)
	proj := map[string]*PlainProjector{}
	get := func(s string) *PlainProjector {
		if p, ok := proj[s]; ok {
			return p
		}
		prefix := ""
		if opt.Prefix {
			prefix = s + "| "
		}
		p := NewPlainProjector(bw, prefix)
		proj[s] = p
		return p
	}
	var order []string
	err := c.Replay(func(ev Event, data []byte) error {
		if ev.T != "chunk" || !wanted(opt.Streams, ev.S) {
			return nil
		}
		if _, ok := proj[ev.S]; !ok {
			order = append(order, ev.S)
		}
		_, err := get(ev.S).Write(data)
		return err
	})
	if err != nil && !errors.Is(err, ErrTruncatedJournal) {
		return err
	}
	for _, s := range order {
		if ferr := proj[s].Flush(); ferr != nil {
			return ferr
		}
	}
	if ferr := bw.Flush(); ferr != nil {
		return ferr
	}
	return err
}

func wanted(list []string, s string) bool {
	if list == nil {
		return true
	}
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
