package groupsync

import (
	"errors"
	"testing"
	"time"
)

func TestStatsOnlyCompleteSuccessfulRunsAdvanceFreshness(t *testing.T) {
	stats := &Stats{}
	stats.Record(Result{}, errors.New("synthetic failure"))
	stats.Record(Result{Skipped: true}, nil)
	if stats.LastSuccess.Load() != 0 || stats.Failure.Load() != 1 || stats.Skipped.Load() != 1 {
		t.Fatal("failed/skipped run looked successful")
	}
	before := time.Now().Unix()
	stats.Record(Result{}, nil)
	if stats.LastSuccess.Load() < before || stats.Success.Load() != 1 {
		t.Fatal("successful run not recorded")
	}
}
