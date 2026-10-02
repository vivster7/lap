// Package term captures the output of a child process through a pseudo
// terminal, pipes, or a mix of both, and stores it as a byte-exact journal
// that can later be replayed as a rendered terminal view, a plain-text
// projection, or an asciicast export.
//
// term never launches processes. A caller (package proc) opens a Session,
// passes the child ends from ChildStdio to the new process, applies
// LaunchAttrs (new session + controlling terminal for PTY modes, new process
// group for pipes), calls CloseChildEnds right after the process started, and
// finally calls Done with a drain deadline.
//
// Artifacts written to Spec.Dir:
//
//	output.bytes   raw bytes of every stream in the order they were read
//	events.jsonl   one JSON object per line: start, chunk, resize, input,
//	               eof, error, incomplete, end (see Event)
//	capture.json   Summary, written atomically when Done finalizes
//
// Derived views (plain.txt, rendered.ansi, views.json) are built on demand
// from those artifacts by Capture and record the renderer version.
package term

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// FormatVersion is the version of the on-disk capture format.
const FormatVersion = 1

// Default terminal geometry used when the Spec leaves it unset and there is
// no user terminal to copy.
const (
	DefaultCols = 80
	DefaultRows = 24
)

// DefaultTerm is the TERM value advertised in PTY modes. It matches what the
// charmbracelet/x/vt emulator implements (xterm-style CSI/SGR, 256 and
// truecolor SGR, alternate screen 1049, DSR/DA replies); see SPIKE.md.
const DefaultTerm = "xterm-256color"

// Mode selects how the child's stdio is connected.
type Mode int

const (
	// PTY connects stdout and stderr to one pseudo terminal. The streams are
	// merged by the kernel; the child sees a terminal and behaves as it would
	// in front of a user.
	PTY Mode = iota
	// Pipes connects stdout and stderr to separate pipes. Stream identity is
	// kept, but only the observed read order is recorded.
	Pipes
	// StdoutPipeStderrPTY sends stdout to a pipe (structured output) and
	// stderr to a pseudo terminal (human-oriented progress and diagnostics).
	StdoutPipeStderrPTY
)

func (m Mode) String() string {
	switch m {
	case PTY:
		return "pty"
	case Pipes:
		return "pipes"
	case StdoutPipeStderrPTY:
		return "stdout-pipe+stderr-pty"
	}
	return fmt.Sprintf("mode(%d)", int(m))
}

// Stdin selects what the child reads from.
type Stdin int

const (
	// StdinNull gives the child /dev/null. Tools that check isatty(0) before
	// prompting will not prompt. A PTY child can still open /dev/tty.
	StdinNull Stdin = iota
	// StdinTTY gives a PTY child the terminal as stdin (PTY mode only).
	StdinTTY
)

// Queries selects how terminal queries written by a PTY child (DSR, DA,
// OSC 10/11/12 color queries, DECRQM) are handled.
type Queries int

const (
	// QueriesAnswer answers queries from a live emulator that follows the PTY
	// stream. This is the default for PTY modes.
	QueriesAnswer Queries = iota
	// QueriesIgnore never writes to the terminal. Children that wait for a
	// reply without a timeout will hang until cancelled.
	QueriesIgnore
)

// Spec describes a capture session.
type Spec struct {
	Mode Mode
	// Cols and Rows are the initial terminal size (PTY modes and rendering).
	Cols, Rows uint16
	// Dir is the artifact directory. It is created if missing and must not
	// already contain a capture.
	Dir string
	// Term is the TERM value the caller should give the child (see Env).
	// Empty means DefaultTerm.
	Term  string
	Stdin Stdin
	// Queries controls terminal query replies in PTY modes.
	Queries Queries
	// Live keeps a live emulator of the PTY stream for display (Screen,
	// Updates) even when queries are ignored.
	Live bool
	// Record is caller metadata stored in the start event, e.g. the color
	// policy given to the child (NO_COLOR, FORCE_COLOR, COLORTERM).
	Record map[string]string

	// ReadSize is the per-read buffer size (default 32 KiB).
	ReadSize int
	// QueueChunks bounds read buffers queued between the readers and the disk
	// writer. A full queue blocks the readers (lossless backpressure on the
	// child). Default 64 (2 MiB with the default ReadSize).
	QueueChunks int
	// LiveQueueBytes bounds bytes queued for the live emulator. When full,
	// bytes are dropped for the live view only (never for the disk record)
	// and the live view is marked desynchronized. Default 1 MiB.
	LiveQueueBytes int

	// wrapOutput lets tests inject storage faults into output.bytes.
	wrapOutput func(io.Writer) io.Writer
}

func (s *Spec) setDefaults() {
	if s.Cols == 0 {
		s.Cols = DefaultCols
	}
	if s.Rows == 0 {
		s.Rows = DefaultRows
	}
	if s.Term == "" {
		s.Term = DefaultTerm
	}
	if s.ReadSize <= 0 {
		s.ReadSize = 32 << 10
	}
	if s.QueueChunks <= 0 {
		s.QueueChunks = 64
	}
	if s.LiveQueueBytes <= 0 {
		s.LiveQueueBytes = 1 << 20
	}
}

// Stream names used in events and summaries.
const (
	StreamPTY    = "pty"    // merged stdout+stderr through one terminal
	StreamStdout = "stdout" // stdout pipe
	StreamStderr = "stderr" // stderr pipe or (StdoutPipeStderrPTY) stderr terminal
)

// How a stream ended.
const (
	EndEOF     = "eof"              // read returned EOF (pipe closed by all writers)
	EndHangup  = "hangup"           // PTY master read returned EIO: every slave fd closed
	EndError   = "error"            // unexpected read error; see Err
	EndOpen    = "open_at_deadline" // still open when the drain deadline expired
	EndAborted = "aborted"          // Close was called before Done
)

// StreamSummary describes one captured stream.
type StreamSummary struct {
	Name string `json:"name"`
	TTY  bool   `json:"tty"`
	// Bytes is the number of bytes read from the stream.
	Bytes int64 `json:"bytes"`
	// Lost is the number of read bytes that did not reach output.bytes.
	Lost int64  `json:"lost,omitempty"`
	End  string `json:"end"`
	Err  string `json:"err,omitempty"`
}

// Range is a byte range of the logical capture (all streams, read order)
// that is missing from output.bytes.
type Range struct {
	Off    int64  `json:"off"`
	N      int64  `json:"n"`
	Reason string `json:"reason"`
}

// Summary is the result of a capture. It is also stored as capture.json.
type Summary struct {
	Version  int             `json:"version"`
	Mode     string          `json:"mode"`
	Term     string          `json:"term"`
	Cols     uint16          `json:"cols"`
	Rows     uint16          `json:"rows"`
	Start    time.Time       `json:"start"`
	Elapsed  time.Duration   `json:"elapsed_ns"`
	Streams  []StreamSummary `json:"streams"`
	Received int64           `json:"received"` // bytes read from all streams
	Stored   int64           `json:"stored"`   // bytes in output.bytes
	// Complete is true only if every stream reached EOF/hangup and every byte
	// read was stored and journaled.
	Complete   bool     `json:"complete"`
	Incomplete []Range  `json:"incomplete,omitempty"`
	Problems   []string `json:"problems,omitempty"`
	// Live view statistics (PTY modes with a live emulator).
	LiveDropped int64 `json:"live_dropped,omitempty"`
	Replies     int   `json:"replies,omitempty"`
	RepliesLost int   `json:"replies_lost,omitempty"`
}

// ErrIncomplete is returned (wrapped) by Done when the capture is not
// complete. The Summary is still valid and says what is missing.
var ErrIncomplete = errors.New("term: capture incomplete")

// ErrUnsupported is returned by Open on platforms without PTY support.
var ErrUnsupported = errors.New("term: unsupported platform")

// LaunchAttrs tells the launcher how to set up the child's session.
type LaunchAttrs struct {
	// Setsid and Setctty: start a new session whose controlling terminal is
	// child fd CttyFD (PTY modes). Do not combine with Setpgid.
	Setsid  bool
	Setctty bool
	CttyFD  int
	// Setpgid: start a new process group (pipe mode).
	Setpgid bool
}

// Child returns the three child-side files, in fd order 0, 1, 2.
func (s *Session) ChildStdio() (stdin, stdout, stderr *os.File) {
	return s.childIn, s.childOut, s.childErr
}

// NeedsCtty reports whether the child must start in a new session with a
// controlling terminal (true for PTY modes).
func (s *Session) NeedsCtty() bool { return s.spec.Mode != Pipes }

// CttyFD is the child fd number to pass as SysProcAttr.Ctty.
func (s *Session) CttyFD() int { return s.cttyFD }

// LaunchAttrs returns the session setup the child needs.
func (s *Session) LaunchAttrs() LaunchAttrs {
	if s.NeedsCtty() {
		return LaunchAttrs{Setsid: true, Setctty: true, CttyFD: s.cttyFD}
	}
	return LaunchAttrs{Setpgid: true}
}

// Env returns environment entries the child should receive. In PTY modes it
// advertises TERM; in pipe mode it returns nothing (inherit the caller's
// policy). Color policy variables such as NO_COLOR are the caller's.
func (s *Session) Env() []string {
	if s.spec.Mode == Pipes {
		return nil
	}
	return []string{"TERM=" + s.spec.Term}
}

// Dir returns the artifact directory.
func (s *Session) Dir() string { return s.spec.Dir }
