package nzbservice

import "sync"

// addSlots bounds how many adds build a tree at once and hands the slots out in
// the order they were asked for, which is the order the queue lists the adds in.
// That order is the point: an item reports the wait of everything ahead of it,
// and a slot going to whoever the runtime happened to wake would make that
// report a guess.
//
// A limit of 0 or less is no limit, which is what an unconfigured service has.
type addSlots struct {
	mu      sync.Mutex
	limit   int
	held    int
	waiting []chan struct{}
}

// acquire waits for a slot and returns what gives it back.
func (a *addSlots) acquire() func() {
	a.mu.Lock()
	if a.limit <= 0 || a.held < a.limit {
		a.held++
		a.mu.Unlock()
		return a.release
	}

	waiter := make(chan struct{})
	a.waiting = append(a.waiting, waiter)
	a.mu.Unlock()

	<-waiter
	return a.release
}

// release passes the slot straight to the add that has been waiting longest,
// rather than freeing it for whoever asks next: a queue where a latecomer can
// overtake is not a queue.
func (a *addSlots) release() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.waiting) > 0 {
		next := a.waiting[0]
		a.waiting = a.waiting[1:]
		close(next)
		return
	}
	a.held--
}

// setLimit changes how many run at once, and starts whatever the new limit has
// room for.
func (a *addSlots) setLimit(limit int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.limit = limit
	for len(a.waiting) > 0 && (a.limit <= 0 || a.held < a.limit) {
		next := a.waiting[0]
		a.waiting = a.waiting[1:]
		a.held++
		close(next)
	}
}

func (a *addSlots) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.limit
}

// state reports the builds running against what is allowed, which is what
// separates a queue held up by this limit from one held up downstream.
func (a *addSlots) state() (held, limit int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.held, a.limit
}
