package audit

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrFilter = errors.New("invalid audit history filter")

type Filter struct {
	Action   string
	ActorID  string
	TargetID string
	Result   string
	From     *time.Time
	To       *time.Time
	Before   int64
	Limit    int
}

type Entry struct {
	ID int64 `json:"id"`
	Event
}

type Page struct {
	Events     []Entry `json:"events"`
	NextBefore int64   `json:"next_before,omitempty"`
}

type reader interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// List uses keyset pagination so concurrent inserts cannot shift history pages.
// Only explicit fields are selectable; there is no arbitrary SQL/search payload.
func List(ctx context.Context, db reader, filter Filter) (Page, error) {
	if filter.Limit == 0 {
		filter.Limit = 100
	}
	if filter.Limit < 1 || filter.Limit > 200 || filter.Before < 0 || len(filter.Action) > 128 || len(filter.TargetID) > 256 || len(filter.ActorID) > 36 || filter.Result != "" && filter.Result != Succeeded && filter.Result != Failed && filter.Result != Refused || filter.From != nil && filter.To != nil && filter.From.After(*filter.To) {
		return Page{}, ErrFilter
	}
	rows, err := db.Query(ctx, `SELECT id,occurred_at,action,actor_type,COALESCE(actor_id::text,''),COALESCE(actor_name,''),COALESCE(actor_host,''),COALESCE(target_type,''),COALESCE(target_id,''),COALESCE(revision,0),result,COALESCE(reason,''),details
 FROM audit_events WHERE ($1='' OR action=$1) AND ($2='' OR actor_id::text=$2) AND ($3='' OR target_id=$3) AND ($4='' OR result=$4)
 AND ($5::timestamptz IS NULL OR occurred_at >= $5) AND ($6::timestamptz IS NULL OR occurred_at <= $6) AND ($7::bigint=0 OR id<$7)
 ORDER BY id DESC LIMIT $8`, filter.Action, filter.ActorID, filter.TargetID, filter.Result, filter.From, filter.To, filter.Before, filter.Limit+1)
	if err != nil {
		return Page{}, errors.New("audit history is unavailable")
	}
	defer rows.Close()
	page := Page{Events: []Entry{}}
	for rows.Next() {
		var entry Entry
		if err := rows.Scan(&entry.ID, &entry.Time, &entry.Action, &entry.Actor.Type, &entry.Actor.ID, &entry.Actor.Name, &entry.Actor.Host, &entry.TargetType, &entry.TargetID, &entry.Revision, &entry.Result, &entry.Reason, &entry.Details); err != nil {
			return Page{}, errors.New("audit history is unavailable")
		}
		if len(page.Events) == filter.Limit {
			page.NextBefore = page.Events[len(page.Events)-1].ID
			break
		}
		page.Events = append(page.Events, entry)
	}
	if rows.Err() != nil {
		return Page{}, errors.New("audit history is unavailable")
	}
	return page, nil
}
