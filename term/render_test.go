//go:build unix

package term

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeCapture writes a capture in the on-disk format directly, so rendering
// can be tested with exact chunk boundaries.
type fakeChunk struct {
	s    string // stream
	data string
	// resize instead of data when cols > 0
	cols, rows uint16
}

func writeCapture(t testing.TB, cols, rows uint16, streams []string, tty []bool, chunks []fakeChunk) *Capture {
	t.Helper()
	dir := t.TempDir()
	var out, ev bytes.Buffer
	enc := func(v any) { b, _ := json.Marshal(v); ev.Write(append(b, '\n')) }
	enc(map[string]any{"t": "start", "ns": 0, "version": FormatVersion, "mode": "pty", "cols": cols, "rows": rows,
		"term": DefaultTerm, "streams": streams, "tty": tty, "wall": "2026-10-01T00:00:00Z"})
	var off int64
	for i, c := range chunks {
		if c.cols > 0 {
			enc(map[string]any{"t": "resize", "ns": i, "off": off, "cols": c.cols, "rows": c.rows})
			continue
		}
		out.WriteString(c.data)
		enc(map[string]any{"t": "chunk", "s": c.s, "off": off, "n": len(c.data), "ns": i * 1000})
		off += int64(len(c.data))
	}
	os.WriteFile(filepath.Join(dir, "output.bytes"), out.Bytes(), 0o644)
	os.WriteFile(filepath.Join(dir, "events.jsonl"), ev.Bytes(), 0o644)
	c, err := OpenCapture(dir)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func ptyCapture(t testing.TB, cols, rows uint16, chunks ...string) *Capture {
	var cs []fakeChunk
	for _, c := range chunks {
		cs = append(cs, fakeChunk{s: StreamPTY, data: c})
	}
	return writeCapture(t, cols, rows, []string{StreamPTY}, []bool{true}, cs)
}

func split(s string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	return append(out, s)
}

func plainText(s string) string {
	var b strings.Builder
	p := NewPlainProjector(&b, "")
	p.Write([]byte(s))
	p.Flush()
	return b.String()
}

// Q4: the rendered view must not depend on how the stream was split into
// reads, including UTF-8 sequences and grapheme clusters split across reads
// (vt alone breaks "e"+U+0301 split across two Writes).
func TestRenderChunkingInvariant(t *testing.T) {
	stream := "progress 10%\rprogress 100%\r\n" +
		"\x1b[31mred\x1b[0m café e\u0301 a界b 😀 👨\u200d👩\u200d👧 🇯🇵|\r\n" +
		"line1\r\nline2\r\n\x1b[1A\x1b[2Kreplaced\r\n"
	var want string
	for _, n := range []int{len(stream), 1, 2, 3, 5, 7, 13, 64} {
		got, _ := renderedOf(t, ptyCapture(t, 80, 24, split(stream, n)...))
		if want == "" {
			want = got
			t.Logf("rendered:\n%s", got)
			continue
		}
		if got != want {
			t.Fatalf("chunk size %d renders differently:\n%q\nvs\n%q", n, got, want)
		}
	}
	txt := plainText(want)
	for _, s := range []string{"progress 100%\n", "red café e\u0301 a界b 😀 👨\u200d👩\u200d👧 🇯🇵|\n", "line1\nreplaced\n"} {
		if !strings.Contains(txt, s) {
			t.Errorf("rendered text lacks %q:\n%s", s, txt)
		}
	}
	if !strings.Contains(want, "\x1b[31mred") {
		t.Errorf("SGR color not kept in rendered view: %q", want)
	}
}

// vt's own behaviour without the feeder, for the record.
func TestVTSplitGraphemeGap(t *testing.T) {
	e := newEmulator(20, 2, nil)
	defer closeEmulator(e)
	e.Write([]byte("e"))
	e.Write([]byte("\u0301|"))
	got := strings.TrimSpace(strings.Split(e.String(), "\n")[0])
	if got == "e\u0301|" {
		t.Skip("vt now joins clusters across writes; feeder holdback may be unnecessary")
	}
	t.Logf("vt with split writes renders %q (combining mark lost); feeder fixes this", got)
}

func TestRenderAltScreen(t *testing.T) {
	got, info := renderedOf(t, ptyCapture(t, 40, 5, "main\r\n\x1b[?1049h\x1b[HTUI SCREEN\x1b[?1049lafter\r\n"))
	if plainText(got) != "main\nafter\n" || info.AltScreenAtEnd {
		t.Fatalf("got %q %+v", got, info)
	}
	got, info = renderedOf(t, ptyCapture(t, 40, 5, "main\r\n\x1b[?1049h\x1b[HTUI SCREEN"))
	if !info.AltScreenAtEnd || plainText(got) != "TUI SCREEN\n"+AltScreenMarker+"\nmain\n" {
		t.Fatalf("got %q %+v", got, info)
	}
}

// Scrollback is streamed out of the emulator; `clear` (ED 2 + ED 3) does not
// erase history from the rendered view.
func TestRenderKeepsHistoryAcrossClear(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&b, "old %d\r\n", i)
	}
	b.WriteString("\x1b[H\x1b[2J\x1b[3Jnew screen\r\n")
	got, _ := renderedOf(t, ptyCapture(t, 40, 5, b.String()))
	txt := plainText(got)
	if !strings.Contains(txt, "old 0\n") || !strings.Contains(txt, "old 9\n") || !strings.HasSuffix(txt, "new screen\n") {
		t.Fatalf("history lost:\n%s", txt)
	}
}

// Q4: very long output renders with bounded memory: scrollback is drained
// after every emulator write.
func TestRenderLongOutputBoundedMemory(t *testing.T) {
	const lines = 100_000
	var b bytes.Buffer
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "line %06d \x1b[32mok\x1b[0m\r\n", i)
	}
	c := ptyCapture(t, 80, 24, split(b.String(), 32<<10)...)
	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)
	var peak uint64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			peak = max(peak, m.HeapInuse)
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	start := time.Now()
	info, err := c.Rendered(&countingWriter{}, RenderOptions{})
	el := time.Since(start)
	close(stop)
	<-sampled
	if err != nil {
		t.Fatal(err)
	}
	if info.ScrollbackLines != lines-23 || info.ScrollbackOverflow != 0 {
		t.Fatalf("info %+v", info)
	}
	growth := float64(peak) - float64(m0.HeapInuse)
	t.Logf("rendered %d lines (%.1f MB) in %v (%.1f MB/s), scrollback lines %d, peak heap growth %.1f MB",
		lines, float64(b.Len())/1e6, el.Round(time.Millisecond), float64(b.Len())/1e6/el.Seconds(), info.ScrollbackLines, growth/1e6)
	if growth > 64e6 {
		t.Fatalf("heap grew %.0f MB", growth/1e6)
	}
}

type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// Pipe streams have bare LF; the renderer sets LNM so lines start at col 0.
func TestRenderPipeStreams(t *testing.T) {
	c := writeCapture(t, 40, 5, []string{StreamStdout, StreamStderr}, []bool{false, false}, []fakeChunk{
		{s: StreamStdout, data: "out1\n"}, {s: StreamStderr, data: "err1\n"}, {s: StreamStdout, data: "out2\n"},
	})
	got, _ := renderedOf(t, c)
	if plainText(got) != "out1\nerr1\nout2\n" {
		t.Fatalf("got %q", got)
	}
	if p := plainOf(t, c, PlainOptions{Prefix: true}); p != "stdout| out1\nstderr| err1\nstdout| out2\n" {
		t.Fatalf("plain %q", p)
	}
}

func TestRenderResizeAndTail(t *testing.T) {
	c := writeCapture(t, 10, 3, []string{StreamPTY}, []bool{true}, []fakeChunk{
		{s: StreamPTY, data: "0123456789abcdef\r\n"},
		{cols: 20, rows: 3},
		{s: StreamPTY, data: "0123456789abcdef\r\n"},
	})
	got, info := renderedOf(t, c)
	if info.Cols != 20 || info.Resizes != 1 {
		t.Fatalf("%+v", info)
	}
	// Before the resize the line wraps at 10 columns; after it, it fits.
	if txt := plainText(got); !strings.Contains(txt, "0123456789\nabcdef\n0123456789abcdef\n") {
		t.Fatalf("got %q", txt)
	}

	var b strings.Builder
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&b, "line %04d\r\n", i)
	}
	c = ptyCapture(t, 40, 5, b.String())
	var out bytes.Buffer
	info, err := c.Rendered(&out, RenderOptions{MaxBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	txt := plainText(out.String())
	if info.SkippedBytes == 0 || !strings.HasPrefix(txt, "line 09") || !strings.HasSuffix(txt, "line 0999\n") {
		t.Fatalf("%+v %q", info, txt[:40])
	}
}

// Q4/Q6: TERM=xterm-256color sequences as emitted by tput render correctly.
func TestRenderTputSequences(t *testing.T) {
	need(t, "tput", "sh")
	_, err, c := run(t, Spec{Mode: PTY}, 5*time.Second, "sh", "-c",
		`tput setaf 1; printf red; tput sgr0; tput cup 3 10; printf X; tput smcup; printf ALT; tput rmcup; tput cr; tput el; printf 'bold?'; tput bold; printf B; tput sgr0`)
	_ = err
	if !strings.Contains(string(rawOf(t, c)), "\x1b[?1049h") {
		t.Fatalf("tput did not use xterm-256color: %q", rawOf(t, c))
	}
	got, _ := renderedOf(t, c)
	lines := strings.Split(plainText(got), "\n")
	if len(lines) < 4 || lines[0] != "red" || lines[3] != "bold?B" {
		t.Fatalf("rendered %q", got)
	}
	if !strings.Contains(got, "\x1b[31mred") || strings.Contains(got, "ALT") {
		t.Fatalf("rendered %q", got)
	}
}

func TestBuildViews(t *testing.T) {
	c := ptyCapture(t, 40, 5, "\x1b[32mok\x1b[0m\r\n10%\r100%\r\n")
	if c.ViewsCurrent() {
		t.Fatal("no views yet")
	}
	vi, err := c.BuildViews(RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !c.ViewsCurrent() || vi.Renderer != RendererID() {
		t.Fatalf("%+v", vi)
	}
	plain, _ := os.ReadFile(filepath.Join(c.Dir, "plain.txt"))
	rendered, _ := os.ReadFile(filepath.Join(c.Dir, "rendered.ansi"))
	if string(plain) != "ok\n100%\n" || !strings.Contains(string(rendered), "\x1b[32mok") {
		t.Fatalf("plain %q rendered %q", plain, rendered)
	}
}
