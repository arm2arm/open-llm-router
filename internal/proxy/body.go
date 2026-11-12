package proxy

import "sync"

// bodyAdmission bounds uploads before model parsing or quota admission. It
// never queues: waiting handlers would themselves be an unbounded resource.
// Only active users have map entries, so cardinality is bounded by maximum.
// Keys and key classes share the same authenticated user's upload budget.
type bodyAdmission struct {
	mu      sync.Mutex
	maximum int
	perUser int
	active  int
	users   map[string]int
}

func (b *bodyAdmission) acquire(user string, concurrency *int) (*bodySlot, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	limit := b.perUser
	if concurrency != nil {
		limit = min(limit, *concurrency)
	}
	if b.users[user] >= limit {
		return nil, "body_readers_per_user"
	}
	if b.active >= b.maximum {
		return nil, "body_readers"
	}
	b.active++
	b.users[user]++
	return &bodySlot{admission: b, user: user}, ""
}

type bodySlot struct {
	once      sync.Once
	admission *bodyAdmission
	user      string
}

func (s *bodySlot) release() {
	s.once.Do(func() {
		b := s.admission
		b.mu.Lock()
		defer b.mu.Unlock()
		b.active--
		b.users[s.user]--
		if b.users[s.user] == 0 {
			delete(b.users, s.user)
		}
	})
}
