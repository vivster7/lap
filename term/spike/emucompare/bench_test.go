package main

import (
	"bytes"
	"fmt"
	"testing"
)

func blk() []byte {
	var line bytes.Buffer
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&line, "line %06d \x1b[32mok\x1b[0m some more text to make it realistic\r\n", i)
	}
	return line.Bytes()
}

func BenchmarkCharmDefaultScrollback(b *testing.B) {
	c := newCharm(80, 24)
	p := blk()
	b.SetBytes(int64(len(p)))
	for i := 0; i < b.N; i++ {
		c.e.Write(p)
	}
}

func BenchmarkCharmDrainScrollback(b *testing.B) {
	c := newCharm(80, 24)
	p := blk()
	b.SetBytes(int64(len(p)))
	for i := 0; i < b.N; i++ {
		c.e.Write(p)
		c.e.ClearScrollback()
	}
}

func BenchmarkCharmPlainText(b *testing.B) {
	c := newCharm(80, 24)
	p := bytes.Repeat([]byte("plain ascii text without any escapes at all ok\r\n"), 1000)
	b.SetBytes(int64(len(p)))
	for i := 0; i < b.N; i++ {
		c.e.Write(p)
		c.e.ClearScrollback()
	}
}

func BenchmarkHeadless(b *testing.B) {
	h := newHeadless(80, 24)
	p := blk()
	b.SetBytes(int64(len(p)))
	for i := 0; i < b.N; i++ {
		h.t.Write(p)
	}
}
