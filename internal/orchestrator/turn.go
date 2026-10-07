package orchestrator

import (
	"context"
	"sync"
)

// turn runs one deploy at a time: lowest tier first, then first come.
type turn struct {
	mu      sync.Mutex
	busy    bool
	seq     uint64
	waiting []*waiter
}

type waiter struct {
	order int
	seq   uint64
	ready chan struct{}
}

// acquire waits for this caller's turn.
func (t *turn) acquire(ctx context.Context, order int) error {
	t.mu.Lock()
	if !t.busy && len(t.waiting) == 0 {
		t.busy = true
		t.mu.Unlock()
		return nil
	}
	t.seq++
	w := &waiter{order: order, seq: t.seq, ready: make(chan struct{})}
	t.waiting = append(t.waiting, w)
	t.mu.Unlock()

	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		t.mu.Lock()
		defer t.mu.Unlock()
		for i, x := range t.waiting {
			if x == w {
				t.waiting = append(t.waiting[:i], t.waiting[i+1:]...)
				return ctx.Err()
			}
		}
		// It was handed the turn just as ctx ended: pass it on.
		t.handOff()
		return ctx.Err()
	}
}

// release ends the current turn and hands it to the next waiter.
func (t *turn) release() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.handOff()
}

// handOff gives the turn to the best waiter, or frees it.
func (t *turn) handOff() {
	if len(t.waiting) == 0 {
		t.busy = false
		return
	}
	best := 0
	for i, w := range t.waiting {
		b := t.waiting[best]
		if w.order < b.order || w.order == b.order && w.seq < b.seq {
			best = i
		}
	}
	w := t.waiting[best]
	t.waiting = append(t.waiting[:best], t.waiting[best+1:]...)
	t.busy = true
	close(w.ready)
}
