package audit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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
	if filter.ActorID != "" {
		var id pgtype.UUID
		if err := id.Scan(filter.ActorID); err != nil || !id.Valid {
			return Page{}, ErrFilter
		}
	}
	// Include only requested predicates. This keeps actor/target lookup sargable
	// even after prepared statements switch from custom to generic plans.
	var args []any
	where := []string{"true"}
	add := func(predicate string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(predicate, len(args)))
	}
	if filter.Action != "" {
		add("action=$%d", filter.Action)
	}
	if filter.ActorID != "" {
		add("actor_id=$%d::uuid", filter.ActorID)
	}
	if filter.TargetID != "" {
		add("target_id=$%d", filter.TargetID)
	}
	if filter.Result != "" {
		add("result=$%d", filter.Result)
	}
	if filter.From != nil {
		add("occurred_at >= $%d::timestamptz", filter.From)
	}
	if filter.To != nil {
		add("occurred_at <= $%d::timestamptz", filter.To)
	}
	if filter.Before > 0 {
		add("id < $%d", filter.Before)
	}
	args = append(args, filter.Limit+1)
	query := `SELECT id,occurred_at,action,actor_type,COALESCE(actor_id::text,''),COALESCE(actor_name,''),COALESCE(actor_host,''),COALESCE(target_type,''),COALESCE(target_id,''),COALESCE(revision,0),result,COALESCE(reason,''),details FROM audit_events WHERE ` + strings.Join(where, " AND ") + fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))
	rows, err := db.Query(ctx, query, args...)
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
