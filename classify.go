package lap

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/vivster7/lap/store"
	"github.com/vivster7/lap/term"
)

// Finding is one diagnostic reported by a tool.
type Finding struct {
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Col     int    `json:"col,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// Classification is passed to Task.Classify after a task ran to completion
// (it is not called for timeouts or interruptions). Classify may change
// Outcome, Findings and Reason.
type Classification struct {
	Task     *Task
	Variant  Variant
	Mode     Mode
	ExitCode int

	Outcome  store.Outcome
	Findings []Finding
	Reason   string

	captureDir string
	plain      *string
}

// Plain returns the captured output as plain text (ANSI stripped, carriage
// return overwrites resolved). It is read once and cached.
func (c *Classification) Plain() string {
	if c.plain != nil {
		return *c.plain
	}
	s := ""
	if cap, err := term.OpenCapture(c.captureDir); err == nil {
		var b bytes.Buffer
		if err := cap.Plain(&b, term.PlainOptions{}); err == nil {
			s = b.String()
		}
	}
	c.plain = &s
	return s
}

// Stream returns the raw bytes of one captured stream ("stdout", "stderr" or
// "pty"), useful for tools emitting JSON on a stdout pipe.
func (c *Classification) Stream(name string) ([]byte, error) {
	cap, err := term.OpenCapture(c.captureDir)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	err = cap.WriteStream(&b, name)
	return b.Bytes(), err
}

// RawOutput returns all captured bytes in read order.
func (c *Classification) RawOutput() ([]byte, error) {
	f, err := os.Open(filepath.Join(c.captureDir, "output.bytes"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func defaultClassify(c *Classification) {
	switch {
	case c.ExitCode == 0:
		c.Outcome = store.Passed
	case c.ExitCode == 126 || c.ExitCode == 127:
		c.Outcome = store.ToolError
		c.Reason = "command not found or not executable"
	default:
		c.Outcome = store.Findings
	}
}

// fileLineRE matches the common "path:line:col: message" diagnostic shape.
var fileLineRE = regexp.MustCompile(`^([^\s:]+\.[A-Za-z0-9]+):(\d+)(?::(\d+))?:?\s*(.*)$`)

// ParseFileLine extracts "file:line[:col]: message" diagnostics from text.
// Adapters can use it in Classify for tools without structured output.
func ParseFileLine(text string) []Finding {
	var out []Finding
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r ")
		m := fileLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		f := Finding{File: m[1], Message: m[4]}
		f.Line, _ = strconv.Atoi(m[2])
		if m[3] != "" {
			f.Col, _ = strconv.Atoi(m[3])
		}
		out = append(out, f)
	}
	return out
}

// ClassifyFileLine is a Classify func: exit 0 passes; otherwise findings are
// parsed with ParseFileLine. Exit codes listed in errorCodes are tool errors.
func ClassifyFileLine(errorCodes ...int) func(*Classification) {
	return func(c *Classification) {
		defaultClassify(c)
		for _, code := range errorCodes {
			if c.ExitCode == code {
				c.Outcome = store.ToolError
				return
			}
		}
		if c.Outcome == store.Findings {
			c.Findings = ParseFileLine(c.Plain())
		}
	}
}
