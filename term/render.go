package term

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/x/vt"
)

// renderScrollbackCap is the emulator's scrollback capacity between drains.
// feeder writes at most maxFeedSegment bytes per Write, so ordinary output
// (one line per LF) cannot overflow it; pathological scroll sequences (CSI n S
// with large n) can, which is reported as ScrollbackOverflow.
const renderScrollbackCap = 2 * maxFeedSegment

// RenderOptions configures the rendered view.
type RenderOptions struct {
	// Streams to feed; nil means all. Pipe streams are fed with LNM set so
	// a bare LF also returns the cursor to column 0.
	Streams []string
	// MaxBytes limits rendering to the last MaxBytes of the selected
	// streams (0: no limit). vt renders ~5-6 MB/s, so huge captures should
	// render a tail; the skipped head is reported in RenderInfo.
	MaxBytes int64
}

// RenderInfo describes a rendered view.
type RenderInfo struct {
	Renderer           string `json:"renderer"`
	Cols               int    `json:"cols"`
	Rows               int    `json:"rows"`
	Bytes              int64  `json:"bytes"`
	SkippedBytes       int64  `json:"skipped_bytes,omitempty"`
	ScrollbackLines    int64  `json:"scrollback_lines"`
	ScrollbackOverflow int    `json:"scrollback_overflow,omitempty"`
	Resizes            int    `json:"resizes,omitempty"`
	AltScreenAtEnd     bool   `json:"alt_screen_at_end,omitempty"`
	Truncated          bool   `json:"journal_truncated,omitempty"`
	Complete           bool   `json:"capture_complete"`
}

// AltScreenMarker separates the alternate screen (when a capture ends inside
// it) from the main screen in a rendered view.
const AltScreenMarker = "──── alternate screen at end of capture ────"

// Rendered replays the capture through the terminal emulator and writes the
// rendered view to w: every line that scrolled off the top, in order, then the
// final main screen (trailing blank rows trimmed). Lines keep SGR styling and
// hyperlinks as ANSI sequences. Lines are screen rows: long lines appear
// wrapped at the terminal width. If the capture ends inside the alternate
// screen, that screen is written first, followed by AltScreenMarker. Memory
// is bounded: scrollback is drained to w after every emulator write.
func (c *Capture) Rendered(w io.Writer, opt RenderOptions) (RenderInfo, error) {
	cols, rows := int(c.Start.Cols), int(c.Start.Rows)
	if cols == 0 {
		cols = DefaultCols
	}
	if rows == 0 {
		rows = DefaultRows
	}
	info := RenderInfo{Renderer: RendererID(), Cols: cols, Rows: rows}
	if s, ok := c.Summary(); ok {
		info.Complete = s.Complete
	}

	var skip int64
	if opt.MaxBytes > 0 {
		var total int64
		_ = c.Replay(func(ev Event, data []byte) error {
			if ev.T == "chunk" && wanted(opt.Streams, ev.S) {
				total += int64(len(data))
			}
			return nil
		})
		if total > opt.MaxBytes {
			skip = total - opt.MaxBytes
		}
	}

	bw := bufio.NewWriterSize(w, 64<<10)
	e := newEmulator(cols, rows, nil)
	defer closeEmulator(e)
	e.SetScrollbackSize(renderScrollbackCap)
	var werr error
	drain := func() {
		if e.IsAltScreen() {
			// Lines scrolled inside the alternate screen are not history.
			e.ClearScrollback()
			return
		}
		sb := e.Scrollback()
		n := sb.Len()
		if n == 0 {
			return
		}
		if n >= renderScrollbackCap {
			info.ScrollbackOverflow++
		}
		for _, l := range sb.Lines() {
			if werr == nil {
				_, werr = bw.WriteString(l.Render())
			}
			if werr == nil {
				werr = bw.WriteByte('\n')
			}
		}
		info.ScrollbackLines += int64(n)
		e.ClearScrollback()
	}
	fd := &feeder{e: e, drain: drain}
	lnm := false
	var seen int64
	err := c.Replay(func(ev Event, data []byte) error {
		switch ev.T {
		case "resize":
			fd.Flush()
			e.Resize(int(ev.Cols), int(ev.Rows))
			info.Resizes++
			info.Cols, info.Rows = int(ev.Cols), int(ev.Rows)
		case "chunk":
			if !wanted(opt.Streams, ev.S) {
				return nil
			}
			if skip > 0 && seen < skip {
				n := int64(len(data))
				if seen+n <= skip {
					seen += n
					info.SkippedBytes += n
					return nil
				}
				// Start at the first line boundary after the skip point.
				k := skip - seen
				seen += n
				data = data[k:]
				info.SkippedBytes += k
				if i := strings.IndexByte(string(data), '\n'); i >= 0 {
					info.SkippedBytes += int64(i + 1)
					data = data[i+1:]
				}
			}
			if !lnm && !c.ttyStream(ev.S) {
				lnm = true
				fd.Flush()
				e.WriteString("\x1b[20h") // LNM: LF implies CR, like ONLCR
			}
			info.Bytes += int64(len(data))
			fd.Write(data)
		}
		return werr
	})
	if errors.Is(err, ErrTruncatedJournal) {
		info.Truncated = true
		info.Complete = false
		err = nil
	}
	if err != nil {
		return info, err
	}
	fd.Flush()
	drain()
	if e.IsAltScreen() {
		info.AltScreenAtEnd = true
		writeScreen(bw, e)
		bw.WriteString(AltScreenMarker + "\n")
		e.WriteString("\x1b[?1049l")
	}
	writeScreen(bw, e)
	if werr != nil {
		return info, werr
	}
	return info, bw.Flush()
}

func writeScreen(w *bufio.Writer, e *vt.Emulator) {
	lines := strings.Split(e.Render(), "\n")
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(stripSGR(lines[last])) == "" {
		last--
	}
	for _, l := range lines[:last+1] {
		w.WriteString(l)
		w.WriteByte('\n')
	}
}

// stripSGR removes escape sequences for blank-line detection.
func stripSGR(s string) string {
	var b strings.Builder
	p := NewPlainProjector(&b, "")
	p.Write([]byte(s))
	p.Flush()
	return b.String()
}

// ViewsInfo is stored as views.json next to cached derived views.
type ViewsInfo struct {
	Renderer      string     `json:"renderer"`
	PlainRevision int        `json:"plain_revision"`
	Render        RenderInfo `json:"render"`
}

// BuildViews writes plain.txt, rendered.ansi and views.json into the capture
// directory (derived, rebuildable caches). It is not part of the drain path.
func (c *Capture) BuildViews(opt RenderOptions) (ViewsInfo, error) {
	vi := ViewsInfo{Renderer: RendererID(), PlainRevision: PlainRevision}
	if err := writeFileAtomic(filepath.Join(c.Dir, "plain.txt"), func(w io.Writer) error {
		return c.Plain(w, PlainOptions{Prefix: len(c.Start.Streams) > 1})
	}); err != nil {
		return vi, err
	}
	if err := writeFileAtomic(filepath.Join(c.Dir, "rendered.ansi"), func(w io.Writer) error {
		var err error
		vi.Render, err = c.Rendered(w, opt)
		return err
	}); err != nil {
		return vi, err
	}
	b, _ := json.MarshalIndent(vi, "", "  ")
	return vi, os.WriteFile(filepath.Join(c.Dir, "views.json"), append(b, '\n'), 0o644)
}

// ViewsCurrent reports whether cached views exist and match this renderer.
func (c *Capture) ViewsCurrent() bool {
	b, err := os.ReadFile(filepath.Join(c.Dir, "views.json"))
	if err != nil {
		return false
	}
	var vi ViewsInfo
	return json.Unmarshal(b, &vi) == nil && vi.Renderer == RendererID() && vi.PlainRevision == PlainRevision
}

func writeFileAtomic(path string, fn func(io.Writer) error) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
