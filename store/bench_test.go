package store

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

// BenchmarkHistoryColdLoad measures a fresh Store's first query over a
// 30k-attempt history (realistic line sizes).
func BenchmarkHistoryColdLoad(b *testing.B) {
	dir := b.TempDir()
	s := openDir(&testing.T{}, dir, Options{})
	f, err := os.Create(s.HistoryPath())
	if err != nil {
		b.Fatal(err)
	}
	base := time.Now()
	for i := 0; i < 30000; i++ {
		a := attemptJSON{Version: 1, Attempt: Attempt{
			RunID: "20261003T142233Z-ab12cd", AttemptID: fmt.Sprintf("task%d-%d", i%25, i), Task: fmt.Sprintf("task%d", i%25),
			Variant: "full", Phase: "check", Groups: []string{"py"}, Mode: "check", Worktree: "/home/user/src/repo",
			Workers: 4, CPU: 4, Memory: 1 << 30, ScopeAll: true, ScopeBucket: "all",
			Queued: base, Start: base, End: base.Add(time.Second), Outcome: Passed,
			CaptureMode: "pty", CaptureDir: "tasks/x/capture", CaptureComplete: true, MaxRSSKB: 123456,
			UserCPU: time.Second, SysCPU: time.Millisecond, Machine: "laptop", ConfigKey: "0123456789abcdef",
		}}
		line, _ := json.Marshal(a)
		f.Write(append(line, '\n'))
	}
	f.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st := openDir(&testing.T{}, dir, Options{})
		st.Estimate(EstimateQuery{Task: "task3", Variant: "full", ScopeBucket: "all", Machine: "laptop", Workers: 4})
	}
}
