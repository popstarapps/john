package terminalui

import "sync"

// queue is an unbounded, ordered, closable handoff between goroutines.
//
// Both directions of the UI use one. An unbounded queue is deliberate: the
// application must never block, deadlock, or drop a user's prompt because the
// terminal is slow to paint, and the UI must never block because the
// application is busy in an LLM call.
type queue[T any] struct {
	mu     sync.Mutex
	items  []T
	wake   chan struct{}
	closed bool
}

func newQueue[T any]() *queue[T] {
	return &queue[T]{wake: make(chan struct{}, 1)}
}

// push appends a value. It reports false once the queue is closed.
func (q *queue[T]) push(v T) bool {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return false
	}
	q.items = append(q.items, v)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

// close stops the queue. Items already pushed are still delivered.
func (q *queue[T]) close() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// take returns the next value, blocking until one arrives. ok is false when the
// queue is closed and drained.
func (q *queue[T]) take() (v T, ok bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			v = q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return v, true
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			var zero T
			return zero, false
		}
		<-q.wake
	}
}

// actionQueue fans a queue out to the public actions channel, preserving order
// and closing the channel when the UI stops.
type actionQueue struct {
	q    *queue[Action]
	ch   chan Action
	once sync.Once
}

func newActionQueue(buf int) *actionQueue {
	a := &actionQueue{q: newQueue[Action](), ch: make(chan Action, buf)}
	go a.pump()
	return a
}

func (a *actionQueue) pump() {
	defer close(a.ch)
	for {
		v, ok := a.q.take()
		if !ok {
			return
		}
		a.ch <- v
	}
}

func (a *actionQueue) push(act Action) { a.q.push(act) }

// stop closes the queue. The actions channel closes once the backlog has been
// delivered, so callers must drain Actions until it closes.
func (a *actionQueue) stop() {
	a.once.Do(func() { a.q.close() })
}
