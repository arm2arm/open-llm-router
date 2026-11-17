package integration_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/portal"
)

func TestAdminUsageSummaryAndDeletionUseUserIndex(t *testing.T) {
	f := startPortal(t)
	root := f.browser(t)
	root.login(t, "usage-plan-admin", []string{"admins"})
	execSQL(t, f.pool, `INSERT INTO users(issuer,sub,username,email,groups)
 SELECT 'seed-plan',s::text,'plan-user-'||s,'',ARRAY['staff'] FROM generate_series(1,1000) s`)
	month := time.Now().UTC().Format("2006-01") + "-01"
	execSQL(t, f.pool, `INSERT INTO usage_daily(day,user_id,model,input_tokens,output_tokens)
 SELECT $1::date+d,u.id,'model-'||m,10,10 FROM users u
 CROSS JOIN generate_series(0,14) d CROSS JOIN generate_series(1,3) m WHERE issuer='seed-plan'`, month)
	execSQL(t, f.pool, `ANALYZE usage_daily`)
	execSQL(t, f.pool, `ANALYZE users`)
	var target string
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM users WHERE issuer='seed-plan' AND sub='999'`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"EXPLAIN (ANALYZE,FORMAT JSON) " + portal.AdminUsersQuery, []any{"plan-user", month, month}},
		{"EXPLAIN (FORMAT JSON) DELETE FROM usage_daily WHERE user_id=$1", []any{target}},
	} {
		var raw []byte
		if err := f.pool.QueryRow(t.Context(), q.sql, q.args...).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var roots []struct{ Plan json.RawMessage }
		if err := json.Unmarshal(raw, &roots); err != nil {
			t.Fatal(err)
		}
		found := false
		var walk func(json.RawMessage)
		walk = func(b json.RawMessage) {
			var n struct {
				Node     string `json:"Node Type"`
				Relation string `json:"Relation Name"`
				Index    string `json:"Index Name"`
				Plans    []json.RawMessage
			}
			if err := json.Unmarshal(b, &n); err != nil {
				t.Fatal(err)
			}
			if n.Relation == "usage_daily" && n.Node == "Seq Scan" {
				t.Fatalf("unbounded daily usage scan: %s", b)
			}
			if n.Index == "usage_daily_user_day_idx" {
				found = true
			}
			for _, child := range n.Plans {
				walk(child)
			}
		}
		walk(roots[0].Plan)
		if !found {
			t.Fatal("user-leading usage index not used")
		}
	}
	resp, body := root.call(t, "GET", "/portal/api/admin/users?match=plan-user", "", "")
	var out struct {
		Limit   int  `json:"limit"`
		HasMore bool `json:"has_more"`
		Users   []struct {
			TokensMonth int64 `json:"tokens_this_month"`
		}
	}
	if resp.StatusCode != 200 || json.Unmarshal(body, &out) != nil || len(out.Users) != 500 {
		t.Fatal("summary lookup failed", resp.StatusCode, len(out.Users))
	}
	if out.Limit != 500 || !out.HasMore {
		t.Fatal("user cap not reported", out.Limit, out.HasMore)
	}
	for _, u := range out.Users {
		if u.TokensMonth != 900 {
			t.Fatal("incorrect monthly usage", u.TokensMonth)
		}
	}
	resp, body = root.call(t, "GET", "/portal/api/admin/users?match=plan-user-999", "", "")
	if resp.StatusCode != 200 || json.Unmarshal(body, &out) != nil || len(out.Users) != 1 || out.HasMore {
		t.Fatal("user beyond cap cannot be found", resp.StatusCode)
	}
	if err := (auth.Admin{Pool: f.pool}).DeleteUser(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM usage_daily WHERE user_id=$1`, target).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("usage cascade incomplete", remaining, err)
	}
}
