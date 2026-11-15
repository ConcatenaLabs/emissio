package main

// claimLocks joins the steps of a submission that must not interleave with
// another submission of the same claim: the query that the claim is free,
// the evidence check (which waits on the explorer and other services), and
// the insert. Without it, two accounts submitting one txid at the same
// moment both pass the query before either inserts, and both are credited;
// one account submitting two transactions for one task at once gets two
// submissions for it.
//
// A lock is held per key while its holder runs, so submissions of other
// txids and other tasks never wait for each other. The locks live in this
// process: two Emissio processes on one database would not see each
// other's, which is not how it runs (one service, one SQLite database).

import "sync"

type claimLocks struct {
	mu sync.Mutex
	m  map[string]*claimLock
}

type claimLock struct {
	sync.Mutex
	users int // holders and waiters, so the entry is dropped when idle
}

// lock takes the lock for key and returns its release.
func (c *claimLocks) lock(key string) func() {
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]*claimLock{}
	}
	l := c.m[key]
	if l == nil {
		l = &claimLock{}
		c.m[key] = l
	}
	l.users++
	c.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		c.mu.Lock()
		l.users--
		if l.users == 0 {
			delete(c.m, key)
		}
		c.mu.Unlock()
	}
}
