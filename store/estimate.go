package store

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

// EstimateQuery identifies the work to estimate. Machine "" means this host.
// ConfigKey "" matches any config; ScopeBucket "" and Workers 0 match any.
type EstimateQuery struct {
	Task, Variant, ScopeBucket, ConfigKey, Machine string
	Workers                                        int
}

// Estimate is a conservative duration prediction from local history.
type Estimate struct {
	Duration   time.Duration // 0 => unknown
	Samples    int
	LowerBound time.Duration // largest censored sample (timeouts)
	Confidence string        // "none" | "low" | "ok"
	Source     string        // human description, e.g. "p90 of 12 samples (+10%)"
}

// Estimation parameters.
const (
	estimateWindow   = 20   // most recent comparable samples used
	estimateMinOK    = 3    // samples needed for a percentile
	estimateOKMargin = 0.10 // p90 * 1.10
	estimateLowScale = 1.25 // max * 1.25 for 1-2 samples
)

// Estimate predicts a duration from comparable history.
//
// Comparable samples share Task, Variant and Machine (never relaxed) and
// ConfigKey (when given), plus ScopeBucket and Workers. With fewer than 3
// completed samples, Workers is relaxed, then ScopeBucket. If no level
// reaches 3, the most specific level with any samples is used. Only
// uncensored passed/fixed/findings attempts form the distribution;
// non-contended samples are preferred while there are at least 3. From the
// 20 most recent: >= 3 samples give p90 * 1.10 ("ok"), 1-2 give max * 1.25
// ("low"). Timeouts at the chosen level set LowerBound, and the final
// Duration is never below it.
func (s *Store) Estimate(q EstimateQuery) Estimate {
	if q.Machine == "" {
		q.Machine = s.hostname
	}
	type level struct {
		name                string
		matchScope, matchWk bool
		done, censored      []Attempt
	}
	levels := []*level{
		{name: "", matchScope: true, matchWk: true},
		{name: "workers relaxed", matchScope: true},
		{name: "workers and scope relaxed"},
	}
	err := s.scanHistory(func(all []Attempt) {
		for i := range all {
			a := &all[i]
			if a.Task != q.Task || a.Variant != q.Variant || a.Machine != q.Machine ||
				(q.ConfigKey != "" && a.ConfigKey != q.ConfigKey) || a.Duration() <= 0 {
				continue
			}
			isCensored := a.censored()
			if !isCensored && !a.Outcome.Completed() {
				continue
			}
			scopeOK := q.ScopeBucket == "" || a.ScopeBucket == q.ScopeBucket
			wkOK := q.Workers == 0 || a.Workers == q.Workers
			for _, l := range levels {
				if (l.matchScope && !scopeOK) || (l.matchWk && !wkOK) {
					continue
				}
				if isCensored {
					l.censored = append(l.censored, *a)
				} else {
					l.done = append(l.done, *a)
				}
			}
		}
	})
	if err != nil {
		return Estimate{Confidence: "none", Source: "history unreadable: " + err.Error()}
	}

	var chosen *level
	for _, l := range levels {
		if len(l.done) >= estimateMinOK {
			chosen = l
			break
		}
	}
	if chosen == nil {
		for _, l := range levels {
			if len(l.done) > 0 {
				chosen = l
				break
			}
		}
	}
	if chosen == nil {
		for _, l := range levels {
			if len(l.censored) > 0 {
				chosen = l
				break
			}
		}
	}
	if chosen == nil {
		return Estimate{Confidence: "none", Source: "no comparable history"}
	}

	var notes []string
	if chosen.name != "" {
		notes = append(notes, chosen.name)
	}
	samples := chosen.done
	if nc := filterContended(samples, false); len(nc) >= estimateMinOK {
		samples = nc
	} else if len(nc) < len(samples) {
		notes = append(notes, "includes contended samples")
	}
	samples = newest(samples, estimateWindow)

	est := Estimate{Samples: len(samples), Confidence: "none"}
	durs := durations(samples)
	switch {
	case len(durs) >= estimateMinOK:
		est.Duration = scale(percentile(durs, 0.90), 1+estimateOKMargin)
		est.Confidence = "ok"
		est.Source = fmt.Sprintf("p90 of %d samples (+%d%%)", len(durs), int(math.Round(estimateOKMargin*100)))
	case len(durs) > 0:
		est.Duration = scale(slices.Max(durs), estimateLowScale)
		est.Confidence = "low"
		est.Source = fmt.Sprintf("max of %d sample(s) (x%.2f)", len(durs), estimateLowScale)
	}
	// A completed attempt supersedes older timeouts: caches warm up and code
	// changes, so only timeouts newer than the latest completion still bound
	// the estimate from below.
	var latestDone time.Time
	for _, a := range chosen.done {
		if a.Start.After(latestDone) {
			latestDone = a.Start
		}
	}
	var recentCensored []Attempt
	for _, a := range chosen.censored {
		if a.Start.After(latestDone) {
			recentCensored = append(recentCensored, a)
		}
	}
	if c := newest(recentCensored, estimateWindow); len(c) > 0 {
		est.LowerBound = slices.Max(durations(c))
		if est.Duration < est.LowerBound {
			est.Duration = est.LowerBound
			if est.Source == "" {
				est.Source = fmt.Sprintf("timed out after %s (lower bound)", est.LowerBound.Round(time.Millisecond))
			} else {
				notes = append(notes, fmt.Sprintf("raised to timeout lower bound %s", est.LowerBound.Round(time.Millisecond)))
			}
		}
		if est.Confidence == "none" {
			est.Confidence = "low"
		}
	}
	if len(notes) > 0 {
		est.Source += "; " + strings.Join(notes, "; ")
	}
	return est
}

func filterContended(in []Attempt, contended bool) []Attempt {
	var out []Attempt
	for _, a := range in {
		if a.Contended == contended {
			out = append(out, a)
		}
	}
	return out
}

// newest returns the n most recent attempts by start time.
func newest(in []Attempt, n int) []Attempt {
	if len(in) <= n {
		return in
	}
	in = slices.Clone(in)
	sort.SliceStable(in, func(i, j int) bool { return in[i].Start.Before(in[j].Start) })
	return in[len(in)-n:]
}

func durations(in []Attempt) []time.Duration {
	out := make([]time.Duration, len(in))
	for i, a := range in {
		out[i] = a.Duration()
	}
	return out
}

func scale(d time.Duration, f float64) time.Duration {
	return time.Duration(math.Round(float64(d) * f))
}

// percentile is the nearest-rank percentile (p in (0,1]) of ds; 0 if empty.
func percentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	i := int(math.Ceil(p*float64(len(s)))) - 1
	return s[max(0, min(i, len(s)-1))]
}

// TaskStats summarizes the attempts of one task variant.
type TaskStats struct {
	Task, Variant string
	Attempts      int
	Counts        map[Outcome]int
	P50, P90      time.Duration // of completed (passed/fixed/findings) uncensored attempts
	LastRun       time.Time
}

// Stats groups the history matching q by task and variant, sorted by task
// then variant. Percentiles are nearest-rank over completed, uncensored
// attempts; P90 is reported only with at least 3 such attempts (else 0).
func (s *Store) Stats(q Query) ([]TaskStats, error) {
	atts, err := s.History(q)
	if err != nil {
		return nil, err
	}
	type key struct{ task, variant string }
	groups := map[key]*TaskStats{}
	durs := map[key][]time.Duration{}
	for _, a := range atts {
		k := key{a.Task, a.Variant}
		st := groups[k]
		if st == nil {
			st = &TaskStats{Task: a.Task, Variant: a.Variant, Counts: map[Outcome]int{}}
			groups[k] = st
		}
		st.Attempts++
		st.Counts[a.Outcome]++
		if t := a.when(); t.After(st.LastRun) {
			st.LastRun = t
		}
		if a.Outcome.Completed() && !a.censored() && a.Duration() > 0 {
			durs[k] = append(durs[k], a.Duration())
		}
	}
	out := make([]TaskStats, 0, len(groups))
	for k, st := range groups {
		ds := durs[k]
		st.P50 = percentile(ds, 0.50)
		if len(ds) >= estimateMinOK {
			st.P90 = percentile(ds, 0.90)
		}
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Task != out[j].Task {
			return out[i].Task < out[j].Task
		}
		return out[i].Variant < out[j].Variant
	})
	return out, nil
}
