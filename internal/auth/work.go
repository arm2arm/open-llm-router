package auth

import (
	"container/list"
	"errors"
	"sync"
	"time"
)

// MaxCredentialWork bounds cache-miss work and coalesced waiters together.
// API keys and OIDC tokens share this budget; admission never queues.
const MaxCredentialWork = 8

const negativeTTL = 5 * time.Second
const maxNegativeCredentials = 4096
const maxCredentialFailures = 10

var ErrAuthBusy = errors.New("authentication work budget exhausted")
var ErrAuthRateLimited = errors.New("too many authentication failures")

type WorkLimiter struct{ slots chan struct{} }

func NewWorkLimiter() *WorkLimiter {
	return &WorkLimiter{slots: make(chan struct{}, MaxCredentialWork)}
}

func (l *WorkLimiter) TryAcquire() bool {
	select {
	case l.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (l *WorkLimiter) Release() { <-l.slots }

// FailureCache holds only credential digests. A fixed expiry is never extended
// by retries; FIFO eviction bounds memory without clearing all protection when
// an attacker fills it. Generations prevent stale denials after admin changes.
// All invalid states use the same response and failure threshold.
type FailureCache struct {
	mu      sync.Mutex
	entries map[[32]byte]*list.Element
	order   list.List
}

type failureEntry struct {
	hash       [32]byte
	generation uint64
	until      time.Time
	attempts   int
}

func (c *FailureCache) Check(hash [32]byte, generation uint64, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[hash]
	if e == nil {
		return nil
	}
	f := e.Value.(*failureEntry)
	if f.generation != generation || !now.Before(f.until) {
		delete(c.entries, hash)
		c.order.Remove(e)
		return nil
	}
	if f.attempts >= maxCredentialFailures {
		return ErrAuthRateLimited
	}
	f.attempts++
	return ErrInvalidKey
}

func (c *FailureCache) Remember(hash [32]byte, generation uint64, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[[32]byte]*list.Element)
	}
	if e := c.entries[hash]; e != nil {
		f := e.Value.(*failureEntry)
		if f.generation == generation && now.Before(f.until) {
			return
		}
		delete(c.entries, hash)
		c.order.Remove(e)
	}
	if len(c.entries) >= maxNegativeCredentials {
		e := c.order.Front()
		delete(c.entries, e.Value.(*failureEntry).hash)
		c.order.Remove(e)
	}
	f := &failureEntry{hash: hash, generation: generation, until: now.Add(negativeTTL), attempts: 1}
	c.entries[hash] = c.order.PushBack(f)
}

func (c *FailureCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
	c.order.Init()
}
