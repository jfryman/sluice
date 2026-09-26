// Package timing records how long named stages take, for one-line logs
// like "timing: scan 0.52s · auth 0.31s · total 1.20s".
package timing

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

type Stage struct {
	Name string
	D    time.Duration
}

// Recorder is safe for concurrent use; a nil *Recorder records nothing.
type Recorder struct {
	mu     sync.Mutex
	start  time.Time
	stages []Stage
}

// Reset clears recorded stages and restarts the total clock.
func (r *Recorder) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.start, r.stages = time.Now(), nil
}

func (r *Recorder) Add(name string, d time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.start.IsZero() {
		r.start = time.Now().Add(-d)
	}
	r.stages = append(r.stages, Stage{name, d})
}

// Time starts a stage; call the returned func to record it:
//
//	defer rec.Time("scan")()
func (r *Recorder) Time(name string) func() {
	t := time.Now()
	return func() { r.Add(name, time.Since(t)) }
}

func (r *Recorder) Stages() []Stage {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Stage(nil), r.stages...)
}

// String renders "a 0.12s · b 1.30s · total 1.42s" (empty if nothing ran).
func (r *Recorder) String() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.stages) == 0 {
		return ""
	}
	parts := make([]string, 0, len(r.stages)+1)
	for _, s := range r.stages {
		parts = append(parts, fmt.Sprintf("%s %.2fs", s.Name, s.D.Seconds()))
	}
	parts = append(parts, fmt.Sprintf("total %.2fs", time.Since(r.start).Seconds()))
	return strings.Join(parts, " · ")
}
