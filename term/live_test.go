//go:build unix

package term

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Q7: a slow consumer of the live rendered view cannot block draining. The
// live emulator (~5 MB/s) is slower than the PTY (~100+ MB/s): it drops bytes
// for the live view, while the disk record stays complete.
func TestSlowLiveConsumerDoesNotBlockDrain(t *testing.T) {
	const size = 30_000_000
	cmdline := `yes 'some ordinary line of tool output 0123456789' | head -c 30000000`
	measure := func(live bool) (time.Duration, Summary) {
		s := openTest(t, Spec{Mode: PTY, Live: live, Queries: QueriesIgnore})
		var frames atomic.Int64
		stop := make(chan struct{})
		if live {
			go func() {
				for {
					select {
					case <-s.Updates():
						_ = s.Screen()
						frames.Add(1)
						time.Sleep(50 * time.Millisecond) // a very slow display
					case <-stop:
						return
					}
				}
			}()
		}
		start := time.Now()
		cmd := launch(t, s, "sh", "-c", cmdline)
		cmd.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		sum, err := s.Done(ctx)
		el := time.Since(start)
		close(stop)
		if err != nil {
			t.Fatal(err, sum)
		}
		if live {
			t.Logf("live=true: %v, %d display frames, live view dropped %.1f MB", el.Round(time.Millisecond), frames.Load(), float64(sum.LiveDropped)/1e6)
		}
		return el, sum
	}
	base, sum0 := measure(false)
	withLive, sum1 := measure(true)
	t.Logf("drain of %.0f MB (PTY adds CR): live=false %v (%.0f MB/s), live=true %v (%.0f MB/s)",
		float64(sum0.Received)/1e6, base.Round(time.Millisecond), float64(sum0.Received)/1e6/base.Seconds(),
		withLive.Round(time.Millisecond), float64(sum1.Received)/1e6/withLive.Seconds())
	if !sum1.Complete || sum1.Stored < size || sum1.LiveDropped == 0 {
		t.Fatalf("summary %+v", sum1)
	}
	if withLive > 3*base+2*time.Second {
		t.Fatalf("live view slowed draining: %v vs %v", withLive, base)
	}
}

// The live screen reflects the stream when it keeps up.
func TestLiveScreen(t *testing.T) {
	s := openTest(t, Spec{Mode: PTY, Live: true})
	cmd := launch(t, s, "sh", "-c", `printf 'hello\r\n\033[32mgreen\033[0m'; sleep 0.2`)
	deadline := time.After(5 * time.Second)
	for !strings.Contains(s.Screen(), "green") {
		select {
		case <-s.Updates():
		case <-deadline:
			t.Fatalf("screen %q", s.Screen())
		}
	}
	cmd.Wait()
	if !strings.Contains(s.Screen(), "\x1b[32mgreen") {
		t.Fatalf("screen %q", s.Screen())
	}
}
