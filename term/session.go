//go:build unix

package term

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// finalizeGrace bounds how long Done waits, after the drain deadline, for the
// disk writer to journal what was already read and write capture.json.
const finalizeGrace = 250 * time.Millisecond

type itemKind uint8

const (
	itemData itemKind = iota
	itemEnd
	itemResize
	itemInput
)

type item struct {
	kind       itemKind
	st         *stream
	buf        []byte // itemData: pooled buffer; itemInput: reply bytes
	n          int
	ns         int64
	end        string
	err        error
	cols, rows uint16
}

type stream struct {
	name string
	f    *os.File // parent end: pty master or pipe read end
	tty  bool
	read atomic.Int64 // bytes read (reader goroutine)

	// writer goroutine only:
	stored int64
	end    string
	errStr string
}

// Session is an open capture. Methods are safe for concurrent use.
type Session struct {
	spec  Spec
	start time.Time
	wall  time.Time

	childIn, childOut, childErr *os.File
	childEnds                   []*os.File
	childMu                     sync.Mutex
	childClosed                 bool
	cttyFD                      int

	masters []*os.File // pty masters (resize targets); masters[0] answers queries
	streams []*stream
	pool    sync.Pool

	q           chan item
	abort       chan struct{} // closed: stop reading (deadline or Close)
	abortOnce   sync.Once
	abortReason atomic.Value // string end status for open streams
	readersDone chan struct{}
	finished    chan struct{} // closed by the writer after finalize

	live *live

	// writer state (writer goroutine only until finished is closed)
	out      *os.File
	outW     io.Writer
	evf      *os.File
	ev       *bufio.Writer
	evErr    error
	stored   int64 // bytes in output.bytes
	received int64 // bytes handed to the writer
	failed   bool
	failOff  int64
	failErr  error
	dropped  int64 // bytes discarded after a write failure
	evBuf    []byte

	summary   Summary
	closeOnce sync.Once
}

// Open creates the artifact directory and the capture channels. Readers start
// immediately; they block until the child writes.
func Open(spec Spec) (*Session, error) {
	spec.setDefaults()
	if spec.Dir == "" {
		return nil, errors.New("term: Spec.Dir is required")
	}
	if spec.Mode != PTY && spec.Stdin == StdinTTY {
		return nil, errors.New("term: StdinTTY requires PTY mode")
	}
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
		return nil, err
	}
	s := &Session{
		spec:        spec,
		q:           make(chan item, spec.QueueChunks),
		abort:       make(chan struct{}),
		readersDone: make(chan struct{}),
		finished:    make(chan struct{}),
	}
	s.pool.New = func() any { b := make([]byte, spec.ReadSize); return &b }
	ok := false
	defer func() {
		if !ok {
			s.closeAllFiles()
		}
	}()

	var err error
	s.out, err = os.OpenFile(filepath.Join(spec.Dir, "output.bytes"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	s.outW = s.out
	if spec.wrapOutput != nil {
		s.outW = spec.wrapOutput(s.out)
	}
	s.evf, err = os.OpenFile(filepath.Join(spec.Dir, "events.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	s.ev = bufio.NewWriterSize(s.evf, 64<<10)

	devnull := func() (*os.File, error) {
		f, err := os.Open(os.DevNull)
		if err == nil {
			s.childEnds = append(s.childEnds, f)
		}
		return f, err
	}
	openPTY := func(name string) (*os.File, *stream, error) {
		master, slave, err := pty.Open()
		if err != nil {
			return nil, nil, err
		}
		s.childEnds = append(s.childEnds, slave)
		if master, err = pollableMaster(master); err != nil {
			return nil, nil, err
		}
		if err := setWinsize(master, spec.Cols, spec.Rows); err != nil {
			master.Close()
			return nil, nil, err
		}
		s.masters = append(s.masters, master)
		st := &stream{name: name, f: master, tty: true}
		s.streams = append(s.streams, st)
		return slave, st, nil
	}
	openPipe := func(name string) (*os.File, error) {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		s.childEnds = append(s.childEnds, w)
		s.streams = append(s.streams, &stream{name: name, f: r})
		return w, nil
	}

	switch spec.Mode {
	case PTY:
		slave, _, err := openPTY(StreamPTY)
		if err != nil {
			return nil, err
		}
		s.childOut, s.childErr = slave, slave
		s.cttyFD = 1
		if spec.Stdin == StdinTTY {
			s.childIn = slave
			s.cttyFD = 0
		} else if s.childIn, err = devnull(); err != nil {
			return nil, err
		}
	case Pipes:
		if s.childOut, err = openPipe(StreamStdout); err != nil {
			return nil, err
		}
		if s.childErr, err = openPipe(StreamStderr); err != nil {
			return nil, err
		}
		if s.childIn, err = devnull(); err != nil {
			return nil, err
		}
	case StdoutPipeStderrPTY:
		if s.childOut, err = openPipe(StreamStdout); err != nil {
			return nil, err
		}
		if s.childErr, _, err = openPTY(StreamStderr); err != nil {
			return nil, err
		}
		s.cttyFD = 2
		if s.childIn, err = devnull(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("term: unknown mode %d", spec.Mode)
	}

	if len(s.masters) > 0 && (spec.Queries == QueriesAnswer || spec.Live) {
		s.live = newLive(s, spec.Queries == QueriesAnswer)
	}

	s.start = time.Now()
	s.wall = s.start
	s.writeStart()

	var wg sync.WaitGroup
	for _, st := range s.streams {
		wg.Add(1)
		go func() { defer wg.Done(); s.readLoop(st) }()
	}
	go func() { wg.Wait(); close(s.readersDone) }()
	go s.writeLoop()
	ok = true
	return s, nil
}

func (s *Session) closeAllFiles() {
	for _, f := range s.childEnds {
		f.Close()
	}
	for _, st := range s.streams {
		st.f.Close()
	}
	if s.out != nil {
		s.out.Close()
	}
	if s.evf != nil {
		s.evf.Close()
	}
}

// CloseChildEnds closes the parent's copies of the child ends. Call it right
// after the child started (or failed to start). Until it is called, a PTY
// never reports hangup and pipes never report EOF. Idempotent.
func (s *Session) CloseChildEnds() error {
	s.childMu.Lock()
	defer s.childMu.Unlock()
	if s.childClosed {
		return nil
	}
	s.childClosed = true
	var first error
	for _, f := range s.childEnds {
		if err := f.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Resize changes the terminal size (PTY modes). The kernel delivers SIGWINCH
// to the terminal's foreground process group; the resize is journaled at the
// current position of the byte stream.
func (s *Session) Resize(cols, rows uint16) error {
	if len(s.masters) == 0 {
		return errors.New("term: Resize needs a PTY mode")
	}
	for _, m := range s.masters {
		if err := setWinsize(m, cols, rows); err != nil {
			return err
		}
	}
	s.send(item{kind: itemResize, cols: cols, rows: rows, ns: s.mono()})
	return nil
}

func (s *Session) mono() int64 { return int64(time.Since(s.start)) }

// send queues a control item for the writer unless the session is finished.
func (s *Session) send(it item) bool {
	select {
	case s.q <- it:
		return true
	case <-s.finished:
		return false
	}
}

func (s *Session) readLoop(st *stream) {
	for {
		bp := s.pool.Get().(*[]byte)
		n, err := st.f.Read(*bp)
		ns := s.mono()
		if n > 0 {
			st.read.Add(int64(n))
			select {
			case s.q <- item{kind: itemData, st: st, buf: *bp, n: n, ns: ns}:
			case <-s.abort:
				// Deadline passed while the writer was behind: the bytes were
				// read but will not be stored; finalize accounts them as lost.
				return
			}
		} else {
			s.pool.Put(bp)
		}
		if err != nil {
			end, e := s.classify(st, err)
			it := item{kind: itemEnd, st: st, end: end, err: e, ns: s.mono()}
			select {
			case s.q <- it:
			default:
				select {
				case s.q <- it:
				case <-s.abort:
				}
			}
			return
		}
	}
}

func (s *Session) classify(st *stream, err error) (string, error) {
	if errors.Is(err, io.EOF) {
		return EndEOF, nil
	}
	if st.tty && isPTYHangup(err) {
		// Linux: once every slave fd is closed the master read returns EIO
		// after the buffered output has been read. This is the PTY's EOF.
		return EndHangup, nil
	}
	select {
	case <-s.abort:
		if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, os.ErrClosed) {
			r, _ := s.abortReason.Load().(string)
			return r, nil
		}
	default:
	}
	return EndError, err
}

func isPTYHangup(err error) bool { return errors.Is(err, syscall.EIO) }

// stopReading interrupts blocked reads. On Linux the PTY master and pipe read
// ends are in the runtime poller, so a past read deadline returns at once.
// Where a deadline is unsupported (darwin PTY master opened blocking) the
// reader stays blocked until the slave closes; it is abandoned.
func (s *Session) stopReading(reason string) {
	s.abortOnce.Do(func() {
		s.abortReason.Store(reason)
		close(s.abort)
		for _, st := range s.streams {
			_ = st.f.SetReadDeadline(time.Now())
		}
	})
}

// Drained is closed when every stream has reached EOF or hangup (or reading
// was stopped). A launcher can wait on it for a short tail grace after the
// child exits, terminate the process group if it is not closed (a descendant
// still holds the terminal or pipe), and then call Done with the cleanup
// allowance.
func (s *Session) Drained() <-chan struct{} { return s.readersDone }

// Done waits until every stream is drained to disk, or until ctx expires (the
// drain deadline). After the deadline the still-open streams are recorded as
// open_at_deadline, their unread tail is unknown, and the capture is marked
// incomplete. Done closes the child ends if the caller has not. It returns
// the Summary and, when the capture is not complete, an error wrapping
// ErrIncomplete.
func (s *Session) Done(ctx context.Context) (Summary, error) {
	s.CloseChildEnds()
	select {
	case <-s.finished:
	case <-ctx.Done():
		s.stopReading(EndOpen)
		select {
		case <-s.finished:
		case <-time.After(finalizeGrace):
			// Writer stuck in a disk write; report without waiting.
			return s.stuckSummary(), fmt.Errorf("%w: storage write did not finish", ErrIncomplete)
		}
	}
	sum := s.summary
	if !sum.Complete {
		return sum, fmt.Errorf("%w: %v", ErrIncomplete, sum.Problems)
	}
	return sum, nil
}

func (s *Session) stuckSummary() Summary {
	return Summary{
		Version: FormatVersion, Mode: s.spec.Mode.String(), Term: s.spec.Term,
		Cols: s.spec.Cols, Rows: s.spec.Rows, Start: s.wall,
		Elapsed: time.Since(s.start), Complete: false,
		Problems: []string{"storage writer did not finish within the drain deadline"},
	}
}

// Close releases the session. If Done has not finalized the capture, reading
// stops, the capture is finalized as aborted where possible. Closing a PTY
// master hangs up the terminal for any process still using it; it is not a
// substitute for process cleanup.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.CloseChildEnds()
		s.stopReading(EndAborted)
		select {
		case <-s.finished:
		case <-time.After(finalizeGrace):
		}
		for _, st := range s.streams {
			st.f.Close()
		}
		if s.live != nil {
			s.live.stop()
		}
	})
	return nil
}

// ---- writer ----

func (s *Session) writeLoop() {
	defer close(s.finished)
	for {
		select {
		case it := <-s.q:
			s.handle(it)
			continue
		default:
		}
		// Queue empty: make the journal current before blocking. Under load
		// events are flushed in 64 KiB batches; after a crash the journal may
		// trail output.bytes, which readers report as an unjournaled tail.
		s.flushEvents()
		select {
		case it := <-s.q:
			s.handle(it)
		case <-s.readersDone:
			s.drainQueue()
			s.finalize()
			return
		case <-s.abort:
			// Store what was already read (bounded by the queue), then stop.
			s.drainQueue()
			s.finalize()
			return
		}
	}
}

// drainQueue stores items already queued. After an abort it spends at most
// half of finalizeGrace; anything left is accounted as lost by finalize.
func (s *Session) drainQueue() {
	var deadline time.Time
	for {
		select {
		case <-s.abort:
			if deadline.IsZero() {
				deadline = time.Now().Add(finalizeGrace / 2)
			} else if time.Now().After(deadline) {
				return
			}
		default:
		}
		select {
		case it := <-s.q:
			s.handle(it)
		default:
			return
		}
	}
}

func (s *Session) handle(it item) {
	switch it.kind {
	case itemData:
		p := it.buf[:it.n]
		s.received += int64(it.n)
		if !s.failed {
			n, err := s.outW.Write(p)
			if n > 0 {
				s.chunkEvent(it.st.name, s.stored, n, it.ns)
				s.stored += int64(n)
				it.st.stored += int64(n)
			}
			if err == nil && n < len(p) {
				err = io.ErrShortWrite
			}
			if err != nil {
				s.failed, s.failOff, s.failErr = true, s.stored, err
				s.dropped += int64(len(p) - n)
				s.event(map[string]any{"t": "write_error", "ns": it.ns, "off": s.stored, "err": err.Error()})
			}
		} else {
			s.dropped += int64(it.n)
		}
		if s.live != nil && it.st.tty {
			s.live.offer(p)
		}
		b := it.buf
		s.pool.Put(&b)
	case itemEnd:
		it.st.end = it.end
		if it.err != nil {
			it.st.errStr = it.err.Error()
		}
		ev := map[string]any{"t": "eof", "ns": it.ns, "s": it.st.name, "end": it.end}
		if it.err != nil {
			ev["t"] = "error"
			ev["err"] = it.err.Error()
		}
		s.event(ev)
		s.flushEvents()
	case itemResize:
		s.event(map[string]any{"t": "resize", "ns": it.ns, "off": s.stored + s.dropped, "cols": it.cols, "rows": it.rows})
		s.flushEvents()
		if s.live != nil {
			s.live.offerResize(it.cols, it.rows)
		}
	case itemInput:
		s.event(map[string]any{"t": "input", "ns": it.ns, "s": it.st.name, "data": string(it.buf)})
		s.flushEvents()
	}
}

func (s *Session) chunkEvent(name string, off int64, n int, ns int64) {
	if s.evErr != nil {
		return
	}
	b := s.evBuf[:0]
	b = append(b, `{"t":"chunk","s":"`...)
	b = append(b, name...)
	b = append(b, `","off":`...)
	b = strconv.AppendInt(b, off, 10)
	b = append(b, `,"n":`...)
	b = strconv.AppendInt(b, int64(n), 10)
	b = append(b, `,"ns":`...)
	b = strconv.AppendInt(b, ns, 10)
	b = append(b, "}\n"...)
	s.evBuf = b
	if _, err := s.ev.Write(b); err != nil {
		s.evErr = err
	}
}

func (s *Session) event(v map[string]any) {
	if s.evErr != nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		s.evErr = err
		return
	}
	b = append(b, '\n')
	if _, err := s.ev.Write(b); err != nil {
		s.evErr = err
	}
}

func (s *Session) flushEvents() {
	if s.evErr != nil {
		return
	}
	if err := s.ev.Flush(); err != nil {
		s.evErr = err
	}
}

func (s *Session) writeStart() {
	names := make([]string, len(s.streams))
	ttys := make([]bool, len(s.streams))
	for i, st := range s.streams {
		names[i], ttys[i] = st.name, st.tty
	}
	s.event(map[string]any{
		"t": "start", "ns": 0, "version": FormatVersion, "mode": s.spec.Mode.String(),
		"cols": s.spec.Cols, "rows": s.spec.Rows, "term": s.spec.Term,
		"streams": names, "tty": ttys, "wall": s.wall.Format(time.RFC3339Nano),
		"stdin":   map[Stdin]string{StdinNull: "null", StdinTTY: "tty"}[s.spec.Stdin],
		"queries": map[Queries]string{QueriesAnswer: "answer", QueriesIgnore: "ignore"}[s.spec.Queries],
		"record":  s.spec.Record,
	})
	s.flushEvents()
}

func (s *Session) finalize() {
	sum := Summary{
		Version: FormatVersion, Mode: s.spec.Mode.String(), Term: s.spec.Term,
		Cols: s.spec.Cols, Rows: s.spec.Rows, Start: s.wall, Elapsed: time.Since(s.start),
		Stored: s.stored, Complete: true,
	}
	var lostAfterStop int64
	for _, st := range s.streams {
		read := st.read.Load()
		ss := StreamSummary{Name: st.name, TTY: st.tty, Bytes: read, End: st.end, Err: st.errStr}
		if ss.End == "" {
			r, _ := s.abortReason.Load().(string)
			if r == "" {
				r = EndAborted
			}
			ss.End = r
		}
		ss.Lost = read - st.stored
		sum.Received += read
		switch ss.End {
		case EndEOF, EndHangup:
		default:
			sum.Complete = false
			sum.Problems = append(sum.Problems, fmt.Sprintf("stream %s: %s%s", st.name, ss.End, errSuffix(ss.Err)))
		}
		sum.Streams = append(sum.Streams, ss)
	}
	// Bytes read by a reader but never handed to the writer (abort while the
	// queue was full).
	lostAfterStop = sum.Received - s.received
	if s.failed {
		sum.Complete = false
		r := Range{Off: s.failOff, N: s.dropped + lostAfterStop, Reason: "write output.bytes: " + s.failErr.Error()}
		sum.Incomplete = append(sum.Incomplete, r)
		sum.Problems = append(sum.Problems, fmt.Sprintf("storage failed at offset %d; %d bytes drained and discarded", r.Off, r.N))
		s.event(map[string]any{"t": "incomplete", "ns": s.mono(), "off": r.Off, "n": r.N, "reason": r.Reason})
	} else if lostAfterStop > 0 {
		sum.Complete = false
		r := Range{Off: s.stored, N: lostAfterStop, Reason: "read but not stored before drain deadline"}
		sum.Incomplete = append(sum.Incomplete, r)
		s.event(map[string]any{"t": "incomplete", "ns": s.mono(), "off": r.Off, "n": r.N, "reason": r.Reason})
	}
	for _, ss := range sum.Streams {
		if ss.End == EndOpen || ss.End == EndAborted || ss.End == EndError {
			// The tail after this point is unknown (not a known length).
			s.event(map[string]any{"t": "incomplete", "ns": s.mono(), "s": ss.Name, "off": s.stored, "n": -1, "reason": "stream " + ss.End})
		}
	}
	if s.live != nil {
		sum.LiveDropped = s.live.dropped.Load()
		sum.Replies = int(s.live.replies.Load())
		sum.RepliesLost = int(s.live.repliesLost.Load())
	}
	s.event(map[string]any{"t": "end", "ns": s.mono(), "stored": s.stored, "received": sum.Received, "complete": sum.Complete && s.evErr == nil})
	s.flushEvents()
	if s.evErr != nil {
		sum.Complete = false
		sum.Problems = append(sum.Problems, "events.jsonl: "+s.evErr.Error())
	}
	if err := s.out.Close(); err != nil && !s.failed {
		sum.Complete = false
		sum.Problems = append(sum.Problems, "close output.bytes: "+err.Error())
	}
	s.evf.Close()
	if err := writeJSONAtomic(filepath.Join(s.spec.Dir, "capture.json"), sum); err != nil {
		sum.Complete = false
		sum.Problems = append(sum.Problems, "capture.json: "+err.Error())
	}
	s.summary = sum
}

func errSuffix(e string) string {
	if e == "" {
		return ""
	}
	return " (" + e + ")"
}

func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
