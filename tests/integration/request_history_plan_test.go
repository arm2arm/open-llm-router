package integration_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/portal"
)

func TestRequestHistoryPlanBoundsLightUsersAndForwarders(t *testing.T) {
	f := startPortal(t)
	lightBrowser := f.browser(t)
	light := lightBrowser.login(t, "history-light-user", []string{"staff"})
	store := auth.Admin{Pool: f.pool}
	recipient, err := store.AddUser(t.Context(), "other-recipient", "", []string{"staff"})
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, f.pool, `INSERT INTO usage(ts,user_id,forwarded_for,model,replica,protocol,status,latency_ms)
 SELECT now()-s*interval '1 second',$1,$2,'bulk','replica','openai','200',1 FROM generate_series(1,100000) s`, f.owner, recipient)
	execSQL(t, f.pool, `INSERT INTO usage(ts,user_id,model,replica,protocol,status,latency_ms)
 SELECT now()-s*interval '1 second',$1,'bulk-direct','replica','openai','200',1 FROM generate_series(1,100000) s`, recipient)
	execSQL(t, f.pool, `ANALYZE usage`)
	type planNode struct {
		Node     string  `json:"Node Type"`
		Relation string  `json:"Relation Name"`
		Index    string  `json:"Index Name"`
		Rows     float64 `json:"Actual Rows"`
		Removed  float64 `json:"Rows Removed by Filter"`
		Plans    []json.RawMessage
	}
	for _, id := range []string{light, f.owner, recipient} {
		var raw []byte
		if err := f.pool.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+portal.RequestHistoryQuery, id, 200).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var root []struct{ Plan json.RawMessage }
		if err := json.Unmarshal(raw, &root); err != nil {
			t.Fatal(err)
		}
		indexes := map[string]bool{}
		var walk func(json.RawMessage)
		walk = func(b json.RawMessage) {
			var n planNode
			if err := json.Unmarshal(b, &n); err != nil {
				t.Fatal(err)
			}
			if n.Relation == "usage" && (n.Node == "Seq Scan" || n.Rows+n.Removed > 400) {
				t.Fatalf("light history scans unbounded usage: %s", b)
			}
			if n.Index != "" {
				indexes[n.Index] = true
			}
			for _, child := range n.Plans {
				walk(child)
			}
		}
		walk(root[0].Plan)
		if id == f.owner && !indexes["usage_attributed_direct_ts"] {
			t.Fatalf("forwarder direct-history path is not bounded: %v", indexes)
		}
		if id == recipient && !indexes["usage_attributed_forwarded_ts"] {
			t.Fatalf("newest forwarded history cannot use its index: %v", indexes)
		}
		want := 0
		if id == recipient {
			want = 200
		}
		var count int
		if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM ("+portal.RequestHistoryQuery+") q", id, 200).Scan(&count); err != nil || count != want {
			t.Fatal("another recipient's requests leaked", count, err)
		}
	}
	// The two branches are disjoint, and their merged order matches attribution.
	for i, tc := range []struct {
		model     string
		forwarded bool
	}{{"direct-newest", false}, {"forwarded-middle", true}, {"direct-oldest", false}} {
		owner := light
		var forwarded any
		if tc.forwarded {
			owner = f.owner
			forwarded = light
		}
		execSQL(t, f.pool, `INSERT INTO usage(ts,user_id,forwarded_for,model,replica,protocol,status,latency_ms,request_id)
   VALUES(now()-$1*interval '1 second',$2,$3,$4,'replica','openai','200',1,$5)`, i, owner, forwarded, tc.model, fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1))
	}
	resp, body := lightBrowser.call(t, "GET", "/portal/api/requests", "", "")
	var out struct {
		Requests []struct {
			Model     string
			Forwarded bool
		}
	}
	if resp.StatusCode != 200 || json.Unmarshal(body, &out) != nil {
		t.Fatal("history lookup failed", resp.StatusCode)
	}
	var models []string
	for _, r := range out.Requests {
		models = append(models, r.Model)
	}
	if !slices.Equal(models, []string{"direct-newest", "forwarded-middle", "direct-oldest"}) || !out.Requests[1].Forwarded {
		t.Fatal("history attribution/order changed", models)
	}
}
