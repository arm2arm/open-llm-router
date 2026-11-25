package integration_test

import (
	"encoding/json"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/requesttrace"
)

func TestPersonalTraceStorageStatus(t *testing.T) {
	f := startPortal(t)
	browser := f.browser(t)
	id := browser.login(t, "trace-storage-status", []string{"staff"})
	get := func() requesttrace.Preferences {
		t.Helper()
		r, data := browser.call(t, "GET", "/portal/api/trace-preferences", "", "")
		var p requesttrace.Preferences
		if r.StatusCode != 200 || json.Unmarshal(data, &p) != nil || p.Storage == nil {
			t.Fatal("storage status unavailable", r.StatusCode, string(data))
		}
		return p
	}
	if p := get(); p.Storage.Traces != 0 || p.Storage.Bytes != 0 || p.Storage.NextExpiry != nil || p.Storage.MaxTraces != requesttrace.MaxStoredTraces || p.Storage.MaxBytes != requesttrace.MaxStoredBytes {
		t.Fatal("incorrect empty storage status", p.Storage)
	}
	other := user(t, f.pool, "local", "other-trace-owner")
	execSQL(t, f.pool, `INSERT INTO request_traces(request_id,user_id,started_at,expires_at,detail)
		VALUES ('other-account',$1,now(),now()+interval '1 day','{"private":"unrelated"}')`, other)
	if p := get(); p.Storage.Traces != 0 || p.Storage.Bytes != 0 {
		t.Fatal("storage status included another account", p.Storage)
	}
	// Older installations may already exceed the count cap. Report the actual
	// budget, without removing history or silently changing consent.
	execSQL(t, f.pool, `INSERT INTO request_traces(request_id,user_id,started_at,expires_at,detail)
		SELECT 'own-'||n::text,$1,now(),now()+interval '1 day',jsonb_build_object('text',repeat('x',1024))
		FROM generate_series(1,$2::int) n`, id, requesttrace.MaxStoredTraces+1)
	p := get()
	if p.Enabled || p.UpdatedAt != nil || p.Storage.Traces != requesttrace.MaxStoredTraces+1 || p.Storage.Bytes < 512*1024 || p.Storage.NextExpiry == nil {
		t.Fatal("storage status lost a limit or changed consent", p)
	}
	response, data := browser.call(t, "DELETE", "/portal/api/request-traces", "{}", f.cfg.Portal.BaseURL)
	if response.StatusCode != 204 {
		t.Fatal("clear traces", response.StatusCode, string(data))
	}
	p = get()
	if p.Enabled || p.UpdatedAt != nil || p.Storage.Traces != 0 || p.Storage.Bytes != 0 || p.Storage.NextExpiry != nil {
		t.Fatal("storage status stale after erasure", p)
	}
	var remaining int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM request_traces WHERE user_id=$1`, other).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatal("personal erasure affected another account", remaining, err)
	}
}
