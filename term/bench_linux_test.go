package term

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// Q7: 200 MB through PTY and pipes. Run with
//
//	go test -run XXX -bench Capture200MB -benchtime 1x ./term/
//
// Metrics: MB/s of drained bytes, peak RSS of the test process (VmHWM after
// resetting it), peak Go heap, and parent CPU seconds per 100 MB.
func BenchmarkCapture200MB(b *testing.B) {
	const size = 200 << 20
	gen := `yes 'some ordinary line of tool output 0123456789 abcdefghijklmnopqrstuvwxyz' | head -c ` + strconv.Itoa(size)
	b.Run("baseline-devnull", func(b *testing.B) {
		b.SetBytes(size)
		for i := 0; i < b.N; i++ {
			cmd := exec.Command("sh", "-c", gen+" >/dev/null")
			if err := cmd.Run(); err != nil {
				b.Fatal(err)
			}
		}
	})
	for _, tc := range []struct {
		name string
		spec Spec
	}{
		{"pty", Spec{Mode: PTY, Queries: QueriesIgnore}},
		{"pty+live-queries", Spec{Mode: PTY, Queries: QueriesAnswer}},
		{"pipes", Spec{Mode: Pipes}},
		{"stdout-pipe+stderr-pty", Spec{Mode: StdoutPipeStderrPTY}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				runtime.GC()
				os.WriteFile("/proc/self/clear_refs", []byte("5"), 0) // reset VmHWM
				var peakHeap uint64
				stop := make(chan struct{})
				sampled := make(chan struct{})
				go func() {
					defer close(sampled)
					for {
						var m runtime.MemStats
						runtime.ReadMemStats(&m)
						peakHeap = max(peakHeap, m.HeapInuse)
						select {
						case <-stop:
							return
						case <-time.After(20 * time.Millisecond):
						}
					}
				}()
				var ru0, ru1 syscall.Rusage
				syscall.Getrusage(syscall.RUSAGE_SELF, &ru0)
				spec := tc.spec
				spec.Dir = filepath.Join(b.TempDir(), "cap")
				b.ResetTimer()
				start := time.Now()
				s, err := Open(spec)
				if err != nil {
					b.Fatal(err)
				}
				cmd := launch(b, s, "sh", "-c", gen)
				cmd.Wait()
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				sum, err := s.Done(ctx)
				cancel()
				el := time.Since(start)
				b.StopTimer()
				syscall.Getrusage(syscall.RUSAGE_SELF, &ru1)
				close(stop)
				<-sampled
				s.Close()
				if err != nil || sum.Received < size {
					b.Fatalf("%v %+v", err, sum)
				}
				b.SetBytes(sum.Received)
				cpu := tv(ru1.Utime) + tv(ru1.Stime) - tv(ru0.Utime) - tv(ru0.Stime)
				b.ReportMetric(float64(sum.Received)/1e6/el.Seconds(), "drainMB/s")
				b.ReportMetric(float64(vmHWM())/1e6, "peakRSS_MB")
				b.ReportMetric(float64(peakHeap)/1e6, "peakHeap_MB")
				b.ReportMetric(cpu/(float64(sum.Received)/1e8), "cpu_s/100MB")
				if c, err := OpenCapture(spec.Dir); err == nil {
					chunks := 0
					c.Replay(func(ev Event, _ []byte) error {
						if ev.T == "chunk" {
							chunks++
						}
						return nil
					})
					b.ReportMetric(float64(sum.Received)/float64(chunks), "B/chunk")
				}
				if sum.LiveDropped > 0 {
					b.ReportMetric(float64(sum.LiveDropped)/1e6, "liveDropped_MB")
				}
				if tc.name == "pty" {
					c, _ := OpenCapture(spec.Dir)
					t0 := time.Now()
					if err := c.Plain(&countingWriter{}, PlainOptions{}); err != nil {
						b.Fatal(err)
					}
					b.ReportMetric(float64(sum.Received)/1e6/time.Since(t0).Seconds(), "plainMB/s")
					var tail countingWriter
					t0 = time.Now()
					if _, err := c.Rendered(&tail, RenderOptions{MaxBytes: 8 << 20}); err != nil {
						b.Fatal(err)
					}
					b.ReportMetric(time.Since(t0).Seconds(), "render8MBtail_s")
				}
			}
		})
	}
}

func tv(t syscall.Timeval) float64 { return float64(t.Sec) + float64(t.Usec)/1e6 }

func vmHWM() int64 {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range bytes.Split(b, []byte("\n")) {
		if bytes.HasPrefix(l, []byte("VmHWM:")) {
			f := bytes.Fields(l[6:])
			kb, _ := strconv.ParseInt(string(f[0]), 10, 64)
			return kb * 1024
		}
	}
	return 0
}
