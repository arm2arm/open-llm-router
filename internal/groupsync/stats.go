package groupsync

import (
	"sync/atomic"
	"time"
)

// Stats persists across discovery retries and configuration changes. All values
// are aggregate; no identity, provider URL or credentials become metric labels.
type Stats struct {
	Enabled         atomic.Bool
	IntervalSeconds atomic.Int64
	LastSuccess     atomic.Int64
	Success         atomic.Uint64
	Failure         atomic.Uint64
	Skipped         atomic.Uint64
}

func (s *Stats) Record(result Result, err error) {
	if s == nil {
		return
	}
	if err != nil {
		s.Failure.Add(1)
		return
	}
	if result.Skipped {
		s.Skipped.Add(1)
		return
	}
	s.Success.Add(1)
	s.LastSuccess.Store(time.Now().Unix())
}
