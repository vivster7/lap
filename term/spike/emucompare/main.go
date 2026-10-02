// Command emucompare feeds the same byte fixtures into three Go terminal
// emulators and prints what each one renders. It is a spike artifact kept in
// its own module so the comparison dependencies do not leak into lap's go.mod.
//
//	cd term/spike/emucompare && go run .
package main

import (
	"bytes"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	avt "github.com/ActiveState/vt10x"
	cvt "github.com/charmbracelet/x/vt"
	ht "github.com/danielgatis/go-headless-term"
)

type emu interface {
	name() string
	write(p []byte)
	screen() []string // visible rows, trailing spaces trimmed
	scrollback() int  // lines retained in scrollback (-1 unsupported)
	replies() string  // bytes the emulator wanted to send back to the app
	cursorX() int
}

func (c *charm) cursorX() int    { return c.e.CursorPosition().X }
func (h *headless) cursorX() int { _, x := h.t.CursorPos(); return x }
func (v *vt10x) cursorX() int    { x, _ := v.st.Cursor(); return x }

// ---- charm vt ----
type charm struct {
	e   *cvt.Emulator
	out bytes.Buffer
	ch  chan []byte
}

func newCharm(cols, rows int) *charm {
	c := &charm{e: cvt.NewEmulator(cols, rows), ch: make(chan []byte, 64)}
	go func() { // replies go to an io.Pipe; without a reader Write blocks forever
		buf := make([]byte, 256)
		for {
			n, err := c.e.Read(buf)
			if n > 0 {
				c.ch <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()
	return c
}
func (c *charm) name() string    { return "charmbracelet/x/vt" }
func (c *charm) write(p []byte)  { c.e.Write(p) }
func (c *charm) scrollback() int { return c.e.ScrollbackLen() }
func (c *charm) screen() []string {
	var out []string
	for _, l := range strings.Split(c.e.String(), "\n") {
		out = append(out, strings.TrimRight(l, " "))
	}
	return out
}
func (c *charm) replies() string {
	time.Sleep(20 * time.Millisecond)
	for {
		select {
		case b := <-c.ch:
			c.out.Write(b)
		default:
			return c.out.String()
		}
	}
}

// ---- headless-term ----
type headless struct {
	t   *ht.Terminal
	out bytes.Buffer
	sb  *ht.MemoryScrollback
}

func newHeadless(cols, rows int) *headless {
	h := &headless{sb: ht.NewMemoryScrollback(10000)}
	h.t = ht.New(ht.WithSize(rows, cols), ht.WithScrollback(h.sb), ht.WithPTYWriter(&h.out))
	return h
}
func (h *headless) name() string    { return "danielgatis/go-headless-term" }
func (h *headless) write(p []byte)  { h.t.Write(p) }
func (h *headless) scrollback() int { return h.t.ScrollbackLen() }
func (h *headless) replies() string { return h.out.String() }
func (h *headless) screen() []string {
	var out []string
	for r := 0; r < h.t.Rows(); r++ {
		out = append(out, h.t.LineContent(r))
	}
	return trimTail(out)
}

// ---- ActiveState vt10x ----
type vt10x struct {
	st      avt.State
	t       *avt.VT
	out     bytes.Buffer
	pending []byte
}

func newVT10x(cols, rows int) *vt10x {
	v := &vt10x{}
	v.st.RecordHistory = true
	v.t, _ = avt.New(&v.st, strings.NewReader(""), &v.out)
	v.t.Resize(cols, rows)
	return v
}
func (v *vt10x) name() string { return "ActiveState/vt10x" }
func (v *vt10x) write(p []byte) {
	// vt10x returns a short count on a split rune: the caller must re-feed.
	p = append(v.pending, p...)
	n, _ := v.t.Write(p)
	v.pending = append([]byte(nil), p[n:]...)
}
func (v *vt10x) scrollback() int { _, y := v.st.GlobalCursor(); _, cy := v.st.Cursor(); return y - cy }
func (v *vt10x) replies() string { return v.out.String() }
func (v *vt10x) screen() []string {
	var out []string
	for _, l := range strings.Split(v.st.String(), "\n") {
		out = append(out, strings.TrimRight(l, " "))
	}
	return trimTail(out)
}

func trimTail(s []string) []string {
	for len(s) > 0 && s[len(s)-1] == "" {
		s = s[:len(s)-1]
	}
	return s
}

type fixture struct {
	name   string
	chunks []string
	want   []string // rows that must appear exactly (prefix rows of screen)
}

func main() {
	fixtures := []fixture{
		{"progress-cr", []string{"Progress 10%\rProgress 50%\rProgress 100%\r\nERROR: boom\r\n"},
			[]string{"Progress 100%", "ERROR: boom"}},
		{"cursor-up-erase", []string{"line1\r\nline2\r\n\x1b[1A\x1b[2Kreplaced\r\nnext\r\n"},
			[]string{"line1", "replaced", "next"}},
		{"alt-screen", []string{"main\r\n\x1b[?1049hALTSCREEN\x1b[?1049lafter\r\n"},
			[]string{"main", "after"}},
		{"wide-cjk", []string{"a界b|\r\n"}, []string{"a界b|"}},
		{"col-after-cjk(want 4)", []string{"a界b"}, nil},
		{"col-after-emoji(want 2)", []string{"😀"}, nil},
		{"col-after-zwj(want 2)", []string{"👨‍👩‍👧"}, nil},
		{"col-after-flag(want 2)", []string{"🇯🇵"}, nil},
		{"col-after-e+acute(want 1)", []string{"e\u0301"}, nil},
		{"emoji-zwj", []string{"x👨‍👩‍👧y|\r\n"}, []string{"x👨‍👩‍👧y|"}},
		{"flag", []string{"🇯🇵|\r\n"}, []string{"🇯🇵|"}},
		{"utf8-split-2byte", []string{"caf\xc3", "\xa9|\r\n"}, []string{"café|"}},
		{"utf8-split-4byte", []string{"\xf0\x9f", "\x98\x80|\r\n"}, []string{"😀|"}},
		{"combining-split", []string{"e", "́|\r\n"}, []string{"é|"}},
		{"zwj-split", []string{"👨‍", "👩|\r\n"}, []string{"👨‍👩|"}},
		{"csi-split", []string{"\x1b[3", "1mred\x1b[0m\r\n"}, []string{"red"}},
		{"dsr", []string{"\x1b[6n"}, nil},
		{"da1", []string{"\x1b[c"}, nil},
		{"osc11", []string{"\x1b]11;?\x07"}, nil},
	}
	makers := []func(c, r int) emu{
		func(c, r int) emu { return newCharm(c, r) },
		func(c, r int) emu { return newHeadless(c, r) },
		func(c, r int) emu { return newVT10x(c, r) },
	}
	for _, mk := range makers {
		fmt.Printf("== %s\n", mk(80, 24).name())
		for _, f := range fixtures {
			e := mk(80, 24)
			for _, c := range f.chunks {
				e.write([]byte(c))
			}
			scr := e.screen()
			ok := true
			for i, w := range f.want {
				if i >= len(scr) || scr[i] != w {
					ok = false
				}
			}
			extra := ""
			if f.want == nil {
				extra = fmt.Sprintf(" reply=%q col=%d", e.replies(), e.cursorX())
			}
			fmt.Printf("  %-18s ok=%-5v screen=%q%s\n", f.name, ok, scr, extra)
		}
		// Long output: 200k lines, measure throughput, scrollback and heap.
		e := mk(80, 24)
		var line bytes.Buffer
		for i := 0; i < 1000; i++ {
			fmt.Fprintf(&line, "line %06d \x1b[32mok\x1b[0m some more text to make it realistic\r\n", i)
		}
		blk := line.Bytes()
		runtime.GC()
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		start := time.Now()
		total := 0
		for i := 0; i < 200; i++ {
			e.write(blk)
			total += len(blk)
		}
		el := time.Since(start)
		runtime.GC()
		runtime.ReadMemStats(&m1)
		fmt.Printf("  long-output: %d lines %.1f MB in %v = %.1f MB/s, scrollback=%d, heap delta=%.1f MB\n",
			200000, float64(total)/1e6, el.Round(time.Millisecond), float64(total)/1e6/el.Seconds(), e.scrollback(),
			(float64(m1.HeapInuse)-float64(m0.HeapInuse))/1e6)
		_ = io.Discard
	}
}
