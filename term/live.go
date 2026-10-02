//go:build unix

package term

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/vt"
)

// replyWriteTimeout bounds a query reply written to the PTY master. A child
// that never reads its terminal must not stall the reply goroutine forever.
const replyWriteTimeout = time.Second

type liveItem struct {
	data       []byte
	cols, rows uint16
	resize     bool
}

// live follows the PTY stream with an emulator for display and for answering
// terminal queries. It never blocks the disk writer: offers beyond
// LiveQueueBytes are dropped (and counted), which desynchronizes the live
// view but never the stored record.
type live struct {
	s      *Session
	st     *stream // the PTY stream being followed
	answer bool
	ch     chan liveItem
	queued atomic.Int64
	max    int64

	dropped     atomic.Int64
	replies     atomic.Int64
	repliesLost atomic.Int64

	mu  sync.Mutex
	emu *vt.Emulator
	fd  *feeder

	updates chan struct{}
	replyCh chan []byte
	stopped chan struct{}
	once    sync.Once
}

func newLive(s *Session, answer bool) *live {
	l := &live{
		s:       s,
		answer:  answer,
		ch:      make(chan liveItem, 4096),
		max:     int64(s.spec.LiveQueueBytes),
		updates: make(chan struct{}, 1),
		replyCh: make(chan []byte, 64),
		stopped: make(chan struct{}),
	}
	for _, st := range s.streams {
		if st.tty {
			l.st = st
			break
		}
	}
	l.emu = newEmulator(int(s.spec.Cols), int(s.spec.Rows), l.onReply)
	l.emu.SetScrollbackSize(1) // live view needs the screen only
	l.fd = &feeder{e: l.emu}
	go l.run()
	if answer {
		go l.replyLoop()
	}
	return l
}

func (l *live) offer(p []byte) {
	n := int64(len(p))
	if l.queued.Load()+n > l.max {
		l.dropped.Add(n)
		return
	}
	select {
	case l.ch <- liveItem{data: append([]byte(nil), p...)}:
		l.queued.Add(n)
	default:
		l.dropped.Add(n)
	}
}

func (l *live) offerResize(cols, rows uint16) {
	select {
	case l.ch <- liveItem{resize: true, cols: cols, rows: rows}:
	default:
	}
}

func (l *live) run() {
	for {
		select {
		case it := <-l.ch:
			l.mu.Lock()
			if it.resize {
				l.emu.Resize(int(it.cols), int(it.rows))
			} else {
				l.fd.Write(it.data)
				if len(l.ch) == 0 {
					// Idle: show held printable text too (e.g. a progress
					// line without a trailing control byte).
					l.fd.Flush()
				}
			}
			l.mu.Unlock()
			l.queued.Add(-int64(len(it.data)))
			select {
			case l.updates <- struct{}{}:
			default:
			}
		case <-l.stopped:
			return
		}
	}
}

// onReply runs on the emulator's reply goroutine. It must not block: the
// emulator Write that produced the reply waits for it.
func (l *live) onReply(b []byte) {
	if !l.answer {
		return
	}
	select {
	case l.replyCh <- b:
	default:
		l.repliesLost.Add(1)
	}
}

func (l *live) replyLoop() {
	m := l.s.masters[0]
	for {
		select {
		case b := <-l.replyCh:
			_ = m.SetWriteDeadline(time.Now().Add(replyWriteTimeout))
			if _, err := m.Write(b); err != nil {
				l.repliesLost.Add(1)
				continue
			}
			l.replies.Add(1)
			l.s.send(item{kind: itemInput, st: l.st, buf: b, ns: l.s.mono()})
		case <-l.stopped:
			return
		}
	}
}

func (l *live) stop() {
	l.once.Do(func() {
		close(l.stopped)
		closeEmulator(l.emu)
	})
}

// Screen returns the live terminal screen (ANSI styled, rows separated by
// "\n"). It reflects the PTY stream up to what the live emulator has
// processed; see Summary.LiveDropped.
func (s *Session) Screen() string {
	if s.live == nil {
		return ""
	}
	s.live.mu.Lock()
	defer s.live.mu.Unlock()
	return s.live.emu.Render()
}

// Updates is signalled (coalesced) whenever the live screen changed. A slow
// consumer only sees fewer updates; it never slows draining.
func (s *Session) Updates() <-chan struct{} {
	if s.live == nil {
		return nil
	}
	return s.live.updates
}

// LiveDropped reports bytes the live emulator skipped so far.
func (s *Session) LiveDropped() int64 {
	if s.live == nil {
		return 0
	}
	return s.live.dropped.Load()
}
