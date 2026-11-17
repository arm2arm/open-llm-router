package requesttrace

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestPerUserCaptureBoundsLeaveCapacityForOthers(t *testing.T) {
	now := time.Now()
	s := &Service{enabled: true, slots: make(chan struct{}, Slots), queue: make(chan *Capture, Slots), now: func() time.Time { return now }}
	prefs := map[string]Preferences{"busy": {Enabled: true}, "other": {Enabled: true}}
	s.prefs.Store(&prefs)
	for range UserSlots {
		if s.Begin("busy", "chat", now) == nil {
			t.Fatal("capture below concurrent limit rejected")
		}
	}
	if s.Begin("busy", "chat", now) != nil || s.Begin("other", "chat", now) == nil {
		t.Fatal("busy account displaced another account")
	}
	for range UserSlots {
		s.release("busy")
	}
	for range UserCapturesPerMinute - UserSlots {
		if s.Begin("busy", "chat", now) == nil {
			t.Fatal("rate budget exhausted early")
		}
		s.release("busy")
	}
	if s.Begin("busy", "chat", now) != nil || s.Dropped.Load() != 2 {
		t.Fatal("per-minute capture limit not enforced")
	}
	now = now.Add(time.Minute)
	if s.Begin("busy", "chat", now) == nil {
		t.Fatal("capture budget did not replenish")
	}
}

func TestCaptureBudgetMapIsBoundedAndReclaimsExpiredEntries(t *testing.T) {
	now := time.Now()
	s := &Service{enabled: true, slots: make(chan struct{}, Slots), now: func() time.Time { return now }}
	prefs := make(map[string]Preferences)
	for i := range maxUserBudgets + 1 {
		prefs[fmt.Sprint(i)] = Preferences{Enabled: true}
	}
	s.prefs.Store(&prefs)
	for i := range maxUserBudgets {
		user := fmt.Sprint(i)
		if s.Begin(user, "chat", now) == nil {
			t.Fatal("unexpected early rejection", i)
		}
		s.release(user)
	}
	if s.Begin(fmt.Sprint(maxUserBudgets), "chat", now) != nil || len(s.budgets) != maxUserBudgets {
		t.Fatal("unbounded per-user budget map")
	}
	now = now.Add(time.Minute)
	if s.Begin(fmt.Sprint(maxUserBudgets), "chat", now) == nil || len(s.budgets) != 1 {
		t.Fatal("expired entries not reclaimed")
	}
}

func TestParametersHaveFinalAggregateBound(t *testing.T) {
	parameters := map[string]any{"seed": json.Number("1234567890123456789"), "model": "test"}
	for i := range 500 {
		parameters[fmt.Sprintf("extension_%04d", i)] = strings.Repeat("ordinary parameter text ", 90)
	}
	raw, _ := json.Marshal(parameters)
	c := &Capture{input: raw}
	d := c.decode()
	check := func(p map[string]any) {
		t.Helper()
		data, _ := json.Marshal(p)
		if len(data) > MaxParameters || p["seed"] != json.Number("1234567890123456789") || p["_olr_omitted_parameters"] == nil {
			t.Fatal("parameters unbounded or ordinary controls lost", len(data), p["seed"])
		}
	}
	check(d.Parameters)
	if !d.Truncated {
		t.Fatal("omission not disclosed")
	}
	// Redaction can expand one-character values and must obey the same cap.
	for i := range 1000 {
		d.Parameters[fmt.Sprintf("extra_%04d", i)] = map[string]any{"password": "a"}
	}
	r, _ := newRedactor(nil)
	encoded, err := r.marshal(d)
	d = Detail{}
	if err != nil || decodeJSON(encoded, &d) != nil {
		t.Fatal(err)
	}
	check(d.Parameters)
}
