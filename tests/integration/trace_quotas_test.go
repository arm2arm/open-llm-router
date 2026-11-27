package integration_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/requesttrace"
	"github.com/arm2arm/open-llm-router/internal/usage"
)

func TestAdminTraceQuotas(t *testing.T) {
	f := startPortal(t)
	admin := f.browser(t)
	adminID := admin.login(t, "quota-admin", []string{"admins"})
	member := f.browser(t)
	id := member.login(t, "quota-member", []string{"staff"})
	global := "/portal/api/admin/trace-quotas"
	personal := "/portal/api/admin/users/" + id + "/trace-quota"
	call := func(path, method, body string, want int) requesttrace.QuotaView {
		t.Helper()
		r, data := admin.call(t, method, path, body, f.cfg.Portal.BaseURL)
		if r.StatusCode != want {
			t.Fatalf("%s %s: %d %s", method, path, r.StatusCode, data)
		}
		var v requesttrace.QuotaView
		if want == 200 && json.Unmarshal(data, &v) != nil {
			t.Fatal("invalid quota response")
		}
		if strings.Contains(string(data), "private-quota-content") {
			t.Fatal("quota API exposed content")
		}
		return v
	}
	for _, path := range []string{global, personal} {
		for _, method := range []string{"GET", "PUT"} {
			r, _ := member.call(t, method, path, `{"version":0,"max_traces":10}`, f.cfg.Portal.BaseURL)
			if r.StatusCode != 403 {
				t.Fatal("non-admin quota access", method, r.StatusCode)
			}
		}
	}
	r, _ := admin.call(t, "PUT", global, `{"version":0,"max_traces":10}`, "")
	if r.StatusCode != 403 {
		t.Fatal("quota mutation without Origin", r.StatusCode)
	}
	if v := call(global, "GET", "", 200); v.Version != 0 || v.Effective.MaxBytes != 64<<20 || v.Effective.MaxTraces != 512 {
		t.Fatal("deployment defaults", v)
	}
	for _, body := range []string{
		`{"max_traces":10}`, `{"version":0,"max_bytes":0}`, `{"version":0,"max_bytes":1048577}`, `{"version":0,"max_bytes":1099512676352}`,
		`{"version":0,"max_traces":0}`, `{"version":0,"max_traces":1000001}`, `{"version":0,"max_traces":1.5}`, `{"version":0,"unknown":1}`,
	} {
		call(global, "PUT", body, 400)
	}
	v := call(global, "PUT", `{"version":0,"max_bytes":33554432,"max_traces":3}`, 200)
	if v.Version != 1 || v.Effective.MaxBytes != 32<<20 || v.Effective.MaxTraces != 3 {
		t.Fatal("save defaults", v)
	}
	call(global, "PUT", `{"version":0,"max_traces":4}`, 409)
	call("/portal/api/admin/users/not-a-uuid/trace-quota", "GET", "", 404)
	call("/portal/api/admin/users/00000000-0000-4000-8000-000000000001/trace-quota", "PUT", `{"version":0,"max_traces":4}`, 404)
	execSQL(t, f.pool, `INSERT INTO request_traces(request_id,user_id,started_at,expires_at,detail) VALUES('quota-private',$1,now(),now()+interval '1 day','{"private":"private-quota-content"}')`, id)
	v = call(personal, "PUT", `{"version":0,"max_bytes":2097152,"max_traces":1}`, 200)
	if v.Version != 1 || v.Defaults.MaxBytes != 32<<20 || v.Effective.MaxBytes != 2<<20 || v.Effective.MaxTraces != 1 || v.Storage.Traces != 1 || v.Storage.Bytes == 0 {
		t.Fatal("user override", v)
	}
	p, err := f.traces.Preferences(t.Context(), id)
	if err != nil || p.Enabled || p.RetentionDays != 7 || p.UpdatedAt != nil || p.Storage.MaxBytes != 2<<20 || p.Storage.MaxTraces != 1 {
		t.Fatal("quota changed consent/retention or preferences stale", p, err)
	}
	if other, err := f.traces.Quotas(t.Context(), adminID); err != nil || other.Storage.Traces != 0 || other.Effective.MaxBytes != 32<<20 {
		t.Fatal("quota leaked between accounts", other, err)
	}
	call(personal, "PUT", `{"version":0,"max_traces":9}`, 409)
	v = call(personal, "PUT", `{"version":1,"max_bytes":null,"max_traces":null}`, 200)
	if v.Version != 2 || v.Effective.MaxBytes != 32<<20 || v.Effective.MaxTraces != 3 || v.Storage.Traces != 1 {
		t.Fatal("reset lost data or default", v)
	}
	var events int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action IN ('trace.quota.defaults','trace.quota.user') AND actor_id=$1 AND result='succeeded'`, adminID).Scan(&events); err != nil || events != 3 {
		t.Fatal("quota audit missing", events, err)
	}
	// Fail closed when the append-only audit cannot commit; both global and user
	// writes must roll back, including their optimistic versions.
	execSQL(t, f.pool, `CREATE FUNCTION reject_quota_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action LIKE 'trace.quota.%' THEN RAISE EXCEPTION 'synthetic audit outage'; END IF; RETURN NEW; END; $$; CREATE TRIGGER reject_quota_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_quota_audit()`)
	call(global, "PUT", `{"version":1,"max_traces":20}`, 503)
	call(personal, "PUT", `{"version":2,"max_traces":20}`, 503)
	if v = call(global, "GET", "", 200); v.Version != 1 || v.Effective.MaxTraces != 3 {
		t.Fatal("global quota committed without audit", v)
	}
	if v = call(personal, "GET", "", 200); v.Version != 2 || v.Effective.MaxTraces != 3 {
		t.Fatal("user quota committed without audit", v)
	}
}

func TestTraceQuotaEnforcementAcrossReplicas(t *testing.T) {
	f := startPortal(t)
	browser := f.browser(t)
	id := browser.login(t, "live-trace-quota", []string{"staff"})
	if err := f.traces.Save(t.Context(), id, requesttrace.Preferences{Enabled: true, RetentionDays: 7}); err != nil {
		t.Fatal(err)
	}
	second, err := requesttrace.New(t.Context(), f.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), config.ContentTracing{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	actor := audit.Actor{Type: "user", ID: id}
	save := func(user string, version int64, bytes, count *int64) {
		t.Helper()
		if err := f.traces.SaveQuotas(t.Context(), user, version, requesttrace.QuotaOverride{MaxBytes: bytes, MaxTraces: count}, actor); err != nil {
			t.Fatal(err)
		}
	}
	oneMiB, twoMiB, two, four, six := int64(1<<20), int64(2<<20), int64(2), int64(4), int64(6)
	save("", 0, &oneMiB, &two)
	capture := func(s *requesttrace.Service, name string) {
		t.Helper()
		c := s.Begin(id, "chat/completions", time.Now())
		if c == nil {
			t.Fatal("capture not admitted")
		}
		c.Input([]byte(`{"messages":[{"role":"user","content":"ordinary text"}]}`))
		c.Finish(usage.Record{UserID: id, RequestID: name, StartedAt: time.Now()})
	}
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Go(func() {
			s := f.traces
			if i%2 == 1 {
				s = second
			}
			capture(s, fmt.Sprint("initial-", i))
		})
	}
	wg.Wait()
	processed := func() uint64 { return f.traces.Processed.Load() + second.Processed.Load() }
	waitFor(t, func() bool { return processed() == 3 })
	count := func(want int64) {
		t.Helper()
		v, err := second.Quotas(t.Context(), id)
		if err != nil || v.Storage.Traces != want {
			t.Fatal("stored traces", v, err)
		}
	}
	count(2)
	if f.traces.Written.Load()+second.Written.Load() != 2 || f.traces.Dropped.Load()+second.Dropped.Load() != 1 {
		t.Fatal("replicas overspent quota")
	}
	// A live per-account increase takes precedence over subsequent global changes.
	save(id, 0, nil, &four)
	one := int64(1)
	save("", 1, &oneMiB, &one)
	capture(second, "raised-user")
	waitFor(t, func() bool { return processed() == 4 })
	count(3)
	save(id, 1, nil, nil)
	capture(second, "lowered-global")
	waitFor(t, func() bool { return processed() == 5 })
	count(3)
	save("", 2, &oneMiB, &six)
	capture(second, "raised-global")
	waitFor(t, func() bool { return processed() == 6 })
	count(4)
	// Test byte capacity independently of trace count, without uploading a large
	// fixture through inference. Existing content stays when a quota is exceeded.
	execSQL(t, f.pool, `INSERT INTO request_traces(request_id,user_id,started_at,expires_at,detail) VALUES('large-existing',$1,now(),now()+interval '1 day',jsonb_build_object('padding',repeat('x',$2::int)))`, id, oneMiB)
	capture(second, "byte-denied")
	waitFor(t, func() bool { return processed() == 7 })
	count(5)
	save("", 3, &twoMiB, &six)
	capture(second, "byte-raised")
	waitFor(t, func() bool { return processed() == 8 })
	count(6)
	if f.traces.Failed.Load()+second.Failed.Load() != 0 {
		t.Fatal("quota rejection counted as write failure")
	}
	p, err := second.Preferences(t.Context(), id)
	if err != nil || !p.Enabled || p.RetentionDays != 7 || p.Storage.MaxBytes != twoMiB || p.Storage.MaxTraces != six {
		t.Fatal("replica preferences stale or consent changed", p, err)
	}
	// Reset inherits deployment settings and survives constructing another replica.
	save("", 4, nil, nil)
	custom, err := requesttrace.New(t.Context(), f.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), config.ContentTracing{Enabled: true, MaxStoredMiB: 128, MaxStoredTraces: 1024})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(custom.Close)
	p, err = custom.Preferences(t.Context(), id)
	if err != nil || p.Storage.MaxBytes != 128<<20 || p.Storage.MaxTraces != 1024 || p.Storage.Traces != 6 {
		t.Fatal("deployment fallback", p, err)
	}
}

func TestConcurrentTraceQuotaEdits(t *testing.T) {
	f := startPortal(t)
	id := f.browser(t).login(t, "quota-editor", []string{"admins"})
	actor := audit.Actor{Type: "user", ID: id}
	for _, target := range []string{"", id} {
		outcomes := make(chan error, 2)
		for count := int64(10); count < 12; count++ {
			go func() {
				outcomes <- f.traces.SaveQuotas(t.Context(), target, 0, requesttrace.QuotaOverride{MaxTraces: &count}, actor)
			}()
		}
		a, b := <-outcomes, <-outcomes
		if !(a == nil && errors.Is(b, requesttrace.ErrQuotaConflict) || b == nil && errors.Is(a, requesttrace.ErrQuotaConflict)) {
			t.Fatal("concurrent edits lost a write", a, b)
		}
	}
}
