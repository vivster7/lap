package term

import (
	"io"
	"strconv"
	"unicode"
	"unicode/utf8"
)

// PlainRevision versions the plain projection semantics below. Bump it when
// they change so cached plain views are rebuilt.
const PlainRevision = 1

// maxLineCells bounds the pending line of the plain projection. A longer line
// is committed in pieces so memory stays bounded for output without newlines.
const maxLineCells = 64 << 10

// PlainProjector converts a terminal byte stream into chronological plain
// text. Semantics (revision 1):
//
//   - A line is committed (written, followed by "\n") on LF, VT or FF, on any
//     vertical cursor movement (CSI A B E F H f d) if the pending line is not
//     empty, at Flush, and when it reaches maxLineCells.
//   - The pending line is a row of cells with a cursor. Printable text
//     overwrites cells at the cursor, like a terminal: CR moves the cursor to
//     column 0 (so "10%\r100%" leaves "100%"), BS moves it left, CSI G/`/C/D
//     move it horizontally, CSI K erases (0: to end, 1: to cursor, 2: line),
//     CSI X erases n cells, CSI P deletes, CSI @ inserts blanks.
//   - CRLF therefore commits the line unchanged: PTY ONLCR is normalized.
//   - SGR and every other CSI, OSC (including hyperlink URLs; link text is
//     kept), DCS/SOS/PM/APC strings, other escape sequences and remaining C0
//     controls are removed.
//   - Zero-width code points (combining marks, ZWJ, variation selectors)
//     attach to the previous cell; every other code point is one cell (wide
//     characters are not given two columns). Invalid UTF-8 becomes U+FFFD.
//     A sequence split across writes is reassembled.
//   - Trailing blanks of a committed line are trimmed. Tabs are kept.
//
// Limitations: text overwritten after moving the cursor up (multi-line
// progress displays) appears once per committed version; screen clears and
// absolute positioning are not replayed; alternate-screen content is included
// in order. Use the rendered view for screen fidelity and the raw record for
// exact bytes.
type PlainProjector struct {
	w      io.Writer
	prefix string
	err    error

	cells []pcell
	col   int

	st     pstate
	params []byte
	utf    [utf8.UTFMax]byte
	un     int
	out    []byte
}

type pcell struct {
	r     rune
	extra string // zero-width code points attached to r
}

type pstate uint8

const (
	psGround pstate = iota
	psEsc
	psEscInter
	psCSI
	psOSC
	psOSCEsc
	psStr
	psStrEsc
)

// NewPlainProjector writes committed lines to w, each preceded by prefix.
func NewPlainProjector(w io.Writer, prefix string) *PlainProjector {
	return &PlainProjector{w: w, prefix: prefix}
}

// Write consumes stream bytes. It never fails on content; it returns the
// first error of the underlying writer.
func (p *PlainProjector) Write(b []byte) (int, error) {
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch p.st {
		case psGround:
			if p.un > 0 {
				p.utfByte(c)
				continue
			}
			if c >= 0x20 && c < 0x7f {
				// Fast path for printable ASCII runs.
				j := i
				for j < len(b) && b[j] >= 0x20 && b[j] < 0x7f {
					p.put(rune(b[j]))
					j++
				}
				i = j - 1
				continue
			}
			if c >= 0x80 {
				p.utfByte(c)
				continue
			}
			p.control(c)
		case psEsc:
			switch {
			case c == '[':
				p.st, p.params = psCSI, p.params[:0]
			case c == ']':
				p.st = psOSC
			case c == 'P' || c == 'X' || c == '^' || c == '_':
				p.st = psStr
			case c >= 0x20 && c <= 0x2f:
				p.st = psEscInter
			case c == 0x1b:
				// stay
			case c < 0x20:
				p.control(c)
			default:
				p.st = psGround // ESC final (ESC 7, ESC c, ...): dropped
			}
		case psEscInter:
			if c >= 0x30 && c <= 0x7e {
				p.st = psGround
			} else if c < 0x20 && c != 0x1b {
				p.control(c)
			}
		case psCSI:
			switch {
			case c >= 0x40 && c <= 0x7e:
				p.csi(c)
				p.st = psGround
			case c >= 0x20 && c <= 0x3f:
				if len(p.params) < 64 {
					p.params = append(p.params, c)
				}
			case c == 0x1b:
				p.st = psEsc
			case c == 0x18 || c == 0x1a: // CAN, SUB abort
				p.st = psGround
			default:
				if c < 0x20 {
					p.control(c) // C0 inside CSI is executed
				}
			}
		case psOSC:
			switch c {
			case 0x07:
				p.st = psGround
			case 0x1b:
				p.st = psOSCEsc
			case 0x18, 0x1a:
				p.st = psGround
			}
		case psOSCEsc:
			if c == '\\' {
				p.st = psGround
			} else if c != 0x1b {
				p.st = psOSC
			}
		case psStr:
			switch c {
			case 0x1b:
				p.st = psStrEsc
			case 0x18, 0x1a:
				p.st = psGround
			}
		case psStrEsc:
			if c == '\\' {
				p.st = psGround
			} else if c != 0x1b {
				p.st = psStr
			}
		}
	}
	return len(b), p.err
}

func (p *PlainProjector) utfByte(c byte) {
	if p.un > 0 && (c < 0x80 || c > 0xbf) {
		// Broken sequence: emit replacement and reprocess c.
		p.un = 0
		p.put(utf8.RuneError)
		p.Write([]byte{c})
		return
	}
	p.utf[p.un] = c
	p.un++
	if utf8.FullRune(p.utf[:p.un]) {
		r, _ := utf8.DecodeRune(p.utf[:p.un])
		p.un = 0
		p.put(r)
	} else if p.un == utf8.UTFMax {
		p.un = 0
		p.put(utf8.RuneError)
	}
}

func (p *PlainProjector) control(c byte) {
	switch c {
	case '\n', '\v', '\f':
		p.commit()
	case '\r':
		p.col = 0
	case '\b':
		if p.col > 0 {
			p.col--
		}
	case '\t':
		p.put('\t')
	case 0x1b:
		p.st = psEsc
	}
}

func zeroWidth(r rune) bool {
	return r == 0x200d || unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf) ||
		(r >= 0xfe00 && r <= 0xfe0f)
}

func (p *PlainProjector) put(r rune) {
	if zeroWidth(r) && p.col > 0 && p.col <= len(p.cells) {
		p.cells[p.col-1].extra += string(r)
		return
	}
	if p.col >= maxLineCells {
		p.commit()
	}
	for len(p.cells) < p.col {
		p.cells = append(p.cells, pcell{r: ' '})
	}
	if p.col == len(p.cells) {
		p.cells = append(p.cells, pcell{r: r})
	} else {
		p.cells[p.col] = pcell{r: r}
	}
	p.col++
}

func (p *PlainProjector) param(i, def int) int {
	n, idx := 0, 0
	start := 0
	for k := 0; k <= len(p.params); k++ {
		if k == len(p.params) || p.params[k] == ';' || p.params[k] == ':' {
			if idx == i {
				s := p.params[start:k]
				if len(s) == 0 {
					return def
				}
				v, err := strconv.Atoi(string(s))
				if err != nil || v == 0 {
					return def
				}
				return v
			}
			idx++
			start = k + 1
		}
	}
	_ = n
	return def
}

func (p *PlainProjector) csi(final byte) {
	// Private (?, <, =, >) or intermediate-bearing sequences are not editing.
	if len(p.params) > 0 && p.params[0] >= 0x3c {
		return
	}
	for _, c := range p.params {
		if c >= 0x20 && c <= 0x2f {
			return
		}
	}
	switch final {
	case 'K':
		mode := 0
		if len(p.params) > 0 {
			mode, _ = strconv.Atoi(string(p.params))
		}
		switch mode {
		case 0:
			if p.col < len(p.cells) {
				p.cells = p.cells[:p.col]
			}
		case 1:
			for k := 0; k <= p.col && k < len(p.cells); k++ {
				p.cells[k] = pcell{r: ' '}
			}
		case 2:
			p.cells = p.cells[:0]
		}
	case 'G', '`':
		p.col = p.param(0, 1) - 1
	case 'C':
		p.col += p.param(0, 1)
	case 'D':
		p.col = max(0, p.col-p.param(0, 1))
	case 'X':
		n := p.param(0, 1)
		for k := p.col; k < p.col+n && k < len(p.cells); k++ {
			p.cells[k] = pcell{r: ' '}
		}
	case 'P':
		n := p.param(0, 1)
		if p.col < len(p.cells) {
			end := min(len(p.cells), p.col+n)
			p.cells = append(p.cells[:p.col], p.cells[end:]...)
		}
	case '@':
		n := p.param(0, 1)
		if p.col < len(p.cells) {
			ins := make([]pcell, n)
			for k := range ins {
				ins[k] = pcell{r: ' '}
			}
			p.cells = append(p.cells[:p.col], append(ins, p.cells[p.col:]...)...)
		}
	case 'A', 'B', 'E', 'F', 'd':
		if len(p.cells) > 0 {
			p.commit()
		}
		if final == 'E' || final == 'F' {
			p.col = 0
		}
	case 'H', 'f':
		if len(p.cells) > 0 {
			p.commit()
		}
		p.col = p.param(1, 1) - 1
	}
	if p.col > maxLineCells {
		p.col = maxLineCells
	}
}

func (p *PlainProjector) commit() {
	end := len(p.cells)
	for end > 0 && p.cells[end-1].r == ' ' && p.cells[end-1].extra == "" {
		end--
	}
	out := append(p.out[:0], p.prefix...)
	for _, c := range p.cells[:end] {
		out = utf8.AppendRune(out, c.r)
		out = append(out, c.extra...)
	}
	out = append(out, '\n')
	p.out = out
	if p.err == nil {
		_, p.err = p.w.Write(out)
	}
	p.cells = p.cells[:0]
	p.col = 0
}

// Flush commits a pending non-empty line (end of stream).
func (p *PlainProjector) Flush() error {
	if p.un > 0 {
		p.un = 0
		p.put(utf8.RuneError)
	}
	if len(p.cells) > 0 {
		p.commit()
	}
	return p.err
}
