package retention

import (
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestNextRunAcrossDaysAndDST(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ now, want time.Time }{
		{time.Date(2026, 11, 11, 2, 0, 0, 0, berlin), time.Date(2026, 11, 11, 3, 0, 0, 0, berlin)},
		{time.Date(2026, 11, 11, 3, 0, 0, 0, berlin), time.Date(2026, 11, 12, 3, 0, 0, 0, berlin)},
		{time.Date(2026, 11, 11, 23, 0, 0, 0, berlin), time.Date(2026, 11, 12, 3, 0, 0, 0, berlin)},
		// The night the clocks go back is 25 hours long.
		{time.Date(2026, 10, 24, 4, 0, 0, 0, berlin), time.Date(2026, 10, 25, 3, 0, 0, 0, berlin)},
	} {
		if got := NextRun("03:00", berlin, tc.now); !got.Equal(tc.want) {
			t.Errorf("NextRun(%s) = %s, want %s", tc.now, got, tc.want)
		}
	}
}

func TestCutoffs(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 30, 0, 0, time.UTC)
	usage, day := Cutoffs(config.Retention{UsageDays: 90, UsageDailyDays: 400}, time.UTC, now)
	if !usage.Equal(time.Date(2026, 8, 13, 12, 30, 0, 0, time.UTC)) || day.Format("2006-01-02") != "2025-10-07" {
		t.Fatal(usage, day)
	}
}
