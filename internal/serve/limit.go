package serve

import (
	"sync"
	"time"
)

// limiter bounds one kind of request from one qube: at most perMinute in
// any minute, and at most cap(slots) at once.
type limiter struct {
	perMinute int
	slots     chan struct{}

	mu     sync.Mutex
	recent []time.Time // when the requests of the last minute came
}

func newLimiter(perMinute, slots int) *limiter {
	return &limiter{perMinute: perMinute, slots: make(chan struct{}, slots)}
}

// take admits a request, or says why it can't. The caller runs release
// when the request ends.
func (l *limiter) take(now time.Time) (release func(), refusal string) {
	l.mu.Lock()
	kept := l.recent[:0]
	for _, t := range l.recent {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	l.recent = kept
	full := len(kept) >= l.perMinute
	if !full {
		l.recent = append(l.recent, now)
	}
	l.mu.Unlock()
	if full {
		return nil, "split-gh: rate limit exceeded; try again in a minute"
	}
	select {
	case l.slots <- struct{}{}:
		return func() { <-l.slots }, ""
	default:
		return nil, "split-gh: too many requests at once; try again when one finishes"
	}
}
