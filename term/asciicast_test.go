//go:build unix

package term

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestAsciicastExport(t *testing.T) {
	c := writeCapture(t, 80, 24, []string{StreamPTY}, []bool{true}, []fakeChunk{
		{s: StreamPTY, data: "caf\xc3"}, {s: StreamPTY, data: "\xa9 \xf0\x9f"}, {s: StreamPTY, data: "\x98\x80\r\n"},
		{cols: 100, rows: 30},
		{s: StreamPTY, data: "bad\xff\r\n"},
	})
	var b bytes.Buffer
	info, err := c.WriteAsciicast(&b, nil)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(&b)
	sc.Scan()
	var hdr map[string]any
	if err := json.Unmarshal(sc.Bytes(), &hdr); err != nil || hdr["version"].(float64) != 2 {
		t.Fatalf("header %s", sc.Bytes())
	}
	var text strings.Builder
	var resize string
	for sc.Scan() {
		var ev []any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("bad event %s", sc.Bytes())
		}
		switch ev[1] {
		case "o":
			text.WriteString(ev[2].(string))
		case "r":
			resize = ev[2].(string)
		}
	}
	if text.String() != "café 😀\r\nbad�\r\n" || resize != "100x30" || info.InvalidBytes != 1 {
		t.Fatalf("text %q resize %q info %+v", text.String(), resize, info)
	}
}
