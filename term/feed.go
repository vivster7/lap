package term

import (
	"bytes"
	"io"

	"github.com/charmbracelet/x/vt"
)

// Renderer identifies the emulator used for rendered views. Bump
// rendererRevision when feeding/projection logic changes so cached views are
// rebuilt.
const (
	EmulatorModule   = "github.com/charmbracelet/x/vt"
	EmulatorVersion  = "v0.0.0-20261001101533-953920dd3285"
	rendererRevision = 1
)

// RendererID is recorded next to every derived rendered view.
func RendererID() string {
	return EmulatorModule + "@" + EmulatorVersion + "+lap-term.r" + itoa(rendererRevision)
}

// maxFeedSegment bounds bytes passed to one emulator Write so that scrollback
// drained after each Write stays small (at most maxFeedSegment lines for LF
// output) and memory stays bounded.
const maxFeedSegment = 4 << 10

// maxHold bounds the printable tail held back waiting for a control byte.
const maxHold = 4 << 10

// newEmulator returns a vt emulator whose reply pipe is always drained:
// vt writes query replies (DSR, DA, OSC 11 ...) to an io.Pipe, and without a
// reader its Write blocks forever on the first query. replies receives each
// reply when non-nil; otherwise replies are discarded.
func newEmulator(cols, rows int, replies func([]byte)) *vt.Emulator {
	e := vt.NewEmulator(cols, rows)
	go func() {
		buf := make([]byte, 512)
		for {
			n, err := e.Read(buf)
			if n > 0 && replies != nil {
				replies(append([]byte(nil), buf[:n]...))
			}
			if err != nil {
				return
			}
		}
	}()
	return e
}

// closeEmulator stops the reply goroutine without touching vt's unsynchronized
// closed flag.
func closeEmulator(e *vt.Emulator) {
	if pw, ok := e.InputPipe().(*io.PipeWriter); ok {
		pw.CloseWithError(io.EOF)
	}
}

// feeder writes a byte stream into a vt emulator so that the result does not
// depend on how the stream was split into reads.
//
// vt flushes a pending grapheme cluster at the end of every Write, so a
// cluster split across two writes ("e" + U+0301, an emoji + ZWJ ...) would be
// rendered as separate cells. feeder therefore only writes up to the last C0
// control byte (which terminates any cluster anyway) and holds the printable
// tail until more bytes or EOF arrive. It also splits before ED 3 (CSI 3 J)
// and RIS (ESC c), which erase vt's scrollback, so drain can save the
// scrollback first.
type feeder struct {
	e     *vt.Emulator
	hold  []byte
	drain func() // called after every emulator Write; may be nil
}

func (f *feeder) Write(p []byte) (int, error) {
	n := len(p)
	if len(f.hold) > 0 {
		f.hold = append(f.hold, p...)
		p = f.hold
	}
	i := lastControl(p)
	if i < 0 {
		if len(p) > maxHold {
			f.feed(p)
			f.hold = f.hold[:0]
		} else if len(f.hold) == 0 {
			f.hold = append(f.hold, p...)
		}
		return n, nil
	}
	f.feed(p[:i+1])
	rest := p[i+1:]
	// rest may alias f.hold; copy down.
	f.hold = append(f.hold[:0], rest...)
	return n, nil
}

// Flush writes any held tail (call at end of stream).
func (f *feeder) Flush() {
	if len(f.hold) > 0 {
		f.feed(f.hold)
		f.hold = f.hold[:0]
	}
}

var (
	seqED3 = []byte("\x1b[3J")
	seqRIS = []byte("\x1bc")
)

func (f *feeder) feed(p []byte) {
	for len(p) > 0 {
		seg := p
		cut := len(seg)
		if cut > maxFeedSegment {
			// Prefer to cut right after a control byte.
			if j := lastControl(seg[:maxFeedSegment]); j >= 0 {
				cut = j + 1
			} else {
				cut = maxFeedSegment
			}
		}
		// Split so that a scrollback-erasing sequence starts its own Write.
		for _, seq := range [][]byte{seqED3, seqRIS} {
			if k := bytes.Index(seg[1:cut], seq); k >= 0 && k+1 < cut {
				cut = k + 1
			}
		}
		f.e.Write(p[:cut])
		if f.drain != nil {
			f.drain()
		}
		p = p[cut:]
	}
}

func lastControl(p []byte) int {
	for i := len(p) - 1; i >= 0; i-- {
		if c := p[i]; c < 0x20 || c == 0x7f {
			return i
		}
	}
	return -1
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		b[n] = '-'
	}
	return string(b[n:])
}
