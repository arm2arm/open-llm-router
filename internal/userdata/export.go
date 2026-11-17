// Package userdata exports an attributed user's records without credentials.
package userdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/arm2arm/open-llm-router/internal/requesttrace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("user not found")

// Export streams one consistent, read-only snapshot. Queries explicitly select
// credential metadata; hashes, cookies and password material never enter JSON.
// Usage belongs to forwarded_for when present, otherwise to user_id.
func Export(ctx context.Context, pool *pgxpool.Pool, id string, output io.Writer) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return errors.New("open user export failed")
	}
	defer tx.Rollback(ctx)
	var profile json.RawMessage
	err = tx.QueryRow(ctx, `SELECT to_jsonb(record) FROM (SELECT id,issuer,sub,username,email,groups,enabled,protected_admin,email_verified,synced_at,disabled_at,trace_enabled,trace_retention_days,trace_preferences_set_at FROM users WHERE id=$1) record`, id).Scan(&profile)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return errors.New("read export account failed")
	}
	write := func(data string) error { _, err := io.WriteString(output, data); return err }
	stamp, _ := json.Marshal(time.Now().UTC())
	if err := write(`{"version":1,"exported_at":` + string(stamp) + `,"user":` + string(profile)); err != nil {
		return err
	}
	sections := []struct{ name, query string }{
		{"keys", `SELECT id,prefix,label,class,trusted_forwarder,created,last_used,expires,revoked FROM api_keys WHERE user_id=$1 ORDER BY created,id`},
		{"administration_keys", `SELECT id,parent_id,label,prefix,permissions,allowed_networks,created,expires,revoked,last_used,last_ip FROM admin_api_keys WHERE owner_id=$1 ORDER BY created,id`},
		{"administration_previews", `SELECT c.id,c.operation,c.summary,c.status,c.created,c.expires,c.completed,c.response_status FROM admin_changes c JOIN admin_api_keys k ON k.id=c.key_id WHERE k.owner_id=$1 ORDER BY c.created,c.id`},
		{"overrides", `SELECT field,value,reason,set_by,set_at FROM quota_overrides WHERE user_id=$1 ORDER BY field`},
		{"sessions", `SELECT created,expires FROM sessions WHERE user_id=$1 ORDER BY created`},
		{"known_local_devices", `SELECT created,expires FROM local_auth_devices WHERE user_id=$1 ORDER BY created`},
		{"local_password_metadata", `SELECT must_change_password,changed_at FROM local_credentials WHERE user_id=$1`},
		{"usage", `SELECT ts,key_id,model,replica,protocol,status,input_tokens,output_tokens,latency_ms,ttfb_ms,is_stream,request_id,operation FROM usage WHERE forwarded_for=$1 OR (user_id=$1 AND forwarded_for IS NULL) ORDER BY ts,request_id`},
		{"daily_totals", `SELECT day,model,requests,input_tokens,output_tokens FROM usage_daily WHERE user_id=$1 ORDER BY day,model`},
		{"request_traces", `SELECT request_id,started_at,expires_at,detail FROM request_traces WHERE user_id=$1 ORDER BY started_at,request_id`},
		{"administration_history", `SELECT id,occurred_at,action,actor_type,actor_id,actor_name,actor_host,target_type,target_id,revision,result,reason,details FROM audit_events WHERE actor_id=$1::uuid OR target_id=$1::text OR details->>'user_id'=$1::text OR details->>'owner_id'=$1::text ORDER BY id`},
	}
	for _, section := range sections {
		if err := write(`,"` + section.name + `":[`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT to_jsonb(record) FROM (`+section.query+`) record`, id)
		if err != nil {
			return fmt.Errorf("read user export section %s failed", section.name)
		}
		first := true
		for rows.Next() {
			var row json.RawMessage
			if err := rows.Scan(&row); err != nil {
				rows.Close()
				return errors.New("read user export record failed")
			}
			if section.name == "request_traces" {
				var trace map[string]json.RawMessage
				if err := json.Unmarshal(row, &trace); err != nil {
					rows.Close()
					return errors.New("decode exported trace failed")
				}
				trace["detail"], err = (*requesttrace.Service)(nil).SanitizeDetail(trace["detail"])
				if err != nil {
					rows.Close()
					return errors.New("sanitize exported trace failed")
				}
				row, err = json.Marshal(trace)
				if err != nil {
					rows.Close()
					return errors.New("encode exported trace failed")
				}
			}
			if !first {
				if err := write(","); err != nil {
					rows.Close()
					return err
				}
			}
			if _, err := output.Write(row); err != nil {
				rows.Close()
				return err
			}
			first = false
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return errors.New("read user export records failed")
		}
		if err := write("]"); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.New("complete user export failed")
	}
	return write("}\n")
}
