package term

import (
	"bytes"
	"strings"
	"testing"
)

func TestPlainProjection(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"lines", "a\nb\n", "a\nb\n"},
		{"crlf (onlcr)", "a\r\nb\r\n", "a\nb\n"},
		{"no trailing newline", "a\nb", "a\nb\n"},
		{"cr overwrite", "10%\r50%\r100%\n", "100%\n"},
		{"cr overwrite keeps longer tail like a terminal", "downloading\rdone\n", "doneloading\n"},
		{"cr + erase line", "downloading\r\x1b[Kdone\n", "done\n"},
		{"erase whole line", "abc\x1b[2Kxyz\n", "   xyz\n"},
		{"erase to cursor", "abcdef\r\x1b[2C\x1b[1Kx\n", "  xdef\n"},
		{"backspace", "abc\b\bXY\n", "aXY\n"},
		{"sgr stripped", "\x1b[1;31mred\x1b[0m plain\n", "red plain\n"},
		{"osc title stripped", "\x1b]0;title\x07text\n", "text\n"},
		{"osc 8 hyperlink keeps text", "\x1b]8;;https://x.test\x1b\\link\x1b]8;;\x1b\\\n", "link\n"},
		{"dcs stripped", "a\x1bP1$r0m\x1b\\b\n", "ab\n"},
		{"private modes stripped", "\x1b[?25l\x1b[?1049hx\x1b[?1049l\n", "x\n"},
		{"cursor up commits", "line1\nline2\x1b[1A\x1b[2Kline1b\n", "line1\nline2\nline1b\n"},
		{"absolute positioning commits", "\x1b[1;1Hfoo\x1b[2;1Hbar\n", "foo\nbar\n"},
		{"cha", "abcdef\x1b[3GX\n", "abXdef\n"},
		{"ech", "abcdef\r\x1b[2XZ\n", "Z cdef\n"},
		{"dch/ich", "abcdef\r\x1b[2P\x1b[1@\n", " cdef\n"},
		{"tabs kept", "a\tb\n", "a\tb\n"},
		{"utf8", "café 界 😀\n", "café 界 😀\n"},
		{"combining after cr", "éx\rE\n", "Ex\n"},
		{"invalid utf8", "a\xffb\n", "a�b\n"},
		{"bell and nul dropped", "a\x07\x00b\n", "ab\n"},
		{"trailing blanks trimmed", "abc   \n", "abc\n"},
	}
	for _, c := range cases {
		for _, n := range []int{len(c.in) + 1, 1, 2, 3} {
			var b bytes.Buffer
			p := NewPlainProjector(&b, "")
			for _, ch := range split(c.in, n) {
				p.Write([]byte(ch))
			}
			p.Flush()
			if b.String() != c.want {
				t.Errorf("%s (chunk %d): got %q want %q", c.name, n, b.String(), c.want)
			}
		}
	}
}

func TestPlainLongLineBounded(t *testing.T) {
	var b bytes.Buffer
	p := NewPlainProjector(&b, "")
	chunk := bytes.Repeat([]byte("x"), 1<<16)
	for i := 0; i < 64; i++ { // 4 MiB without a newline
		p.Write(chunk)
	}
	p.Flush()
	if b.Len() != 64<<16+64 || cap(p.cells) > 2*maxLineCells {
		t.Fatalf("len %d cap %d", b.Len(), cap(p.cells))
	}
}

func BenchmarkPlain(b *testing.B) {
	var in bytes.Buffer
	for i := 0; i < 1000; i++ {
		in.WriteString("\x1b[32mPASS\x1b[0m some/package/name_test.go:123 TestSomething (0.01s)\r\n")
	}
	b.SetBytes(int64(in.Len()))
	p := NewPlainProjector(&countingWriter{}, "")
	for i := 0; i < b.N; i++ {
		p.Write(in.Bytes())
	}
}

// Q5: a real progress bar through a PTY: the plain view keeps the final
// percentage and the error printed after it, not the intermediate redraws.
func TestPlainProgressFixture(t *testing.T) {
	need(t, "python3")
	sum, err, c := run(t, Spec{Mode: PTY}, 5e9, "python3", fixture("progress.py"))
	if err != nil {
		t.Fatal(err, sum)
	}
	plain := plainOf(t, c, PlainOptions{})
	want := "building [##########] 100%\ncafé done\nERROR: tests failed: 3\n"
	if plain != want {
		t.Fatalf("plain:\n%q\nwant\n%q", plain, want)
	}
	if strings.Contains(plain, "50%") {
		t.Fatal("intermediate redraw leaked into plain view")
	}
	rendered, _ := renderedOf(t, c)
	if plainText(rendered) != want || !strings.Contains(rendered, "\x1b[1;31mERROR") && !strings.Contains(rendered, "31") {
		t.Fatalf("rendered %q", rendered)
	}
	t.Logf("raw %d bytes -> plain %q", len(rawOf(t, c)), plain)
}
