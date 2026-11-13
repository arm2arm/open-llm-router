package auth

import (
	"context"
	"errors"
	"time"

	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/jackc/pgx/v5"
)

// auditOperation excludes all mutation inputs by construction. Only action,
// stable IDs, counts and explicitly selected non-secret metadata reach history.
func (a Admin) auditOperation(ctx context.Context, action, targetType, targetID string) (Admin, func(error)) {
	actor := a.Actor
	if scoped, ok := audit.ActorFromContext(ctx); ok {
		actor = scoped
	}
	if actor.Type == "" {
		actor = audit.OperatorCLI()
	}
	a.event = &audit.Event{Action: action, Actor: actor, TargetType: targetType, TargetID: targetID, Result: audit.Succeeded}
	return a, func(err error) {
		if err == nil || a.Pool == nil {
			return
		}
		event := *a.event
		event.Result, event.Reason, event.Details = audit.Failed, "administration operation failed", nil
		for _, refusal := range []error{ErrProtectedAdmin, ErrNotFound, ErrInvalidPassword, ErrPasswordPolicy, ErrPasswordReuse, ErrLocalIdentity, ErrLocalUserInput, ErrLocalUserExists, ErrKeyLifetime, ErrKeyLimit, ErrBootstrapConflict} {
			if errors.Is(err, refusal) {
				event.Result, event.Reason = audit.Refused, refusal.Error()
				break
			}
		}
		// An unavailable audit store cannot record its own failure. The mutation
		// still fails and has rolled back; refusal events are best effort only.
		failureCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = audit.Record(failureCtx, a.Pool, event)
	}
}

func (a Admin) recordAudit(ctx context.Context, tx pgx.Tx) error {
	if a.event == nil {
		return nil
	}
	return audit.Record(ctx, tx, *a.event)
}

func (a Admin) transaction(ctx context.Context, mutate func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
		if err := mutate(tx); err != nil {
			return err
		}
		return a.recordAudit(ctx, tx)
	})
}
