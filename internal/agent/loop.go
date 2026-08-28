// Package agent contains the agent's main loop.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"log/slog"
	"sync"
	"time"
)

// DefaultReportInterval is the default report interval. It is not user-configurable,
// but can be increased by the server response (next_report_in, see G3).
const DefaultReportInterval = 60 * time.Second

// Offset returns a deterministic offset within the interval, derived from the token.
// The same token always yields the same second, different tokens yield different ones.
func Offset(token string, interval time.Duration) time.Duration {
	sum := sha256.Sum256([]byte(token))
	sec := int64(interval / time.Second)
	if sec <= 0 {
		return 0
	}
	return time.Duration(int64(binary.BigEndian.Uint64(sum[:8])%uint64(sec))) * time.Second
}

// Loop runs a tick at an interval that can be changed at runtime.
type Loop struct {
	log   *slog.Logger
	token string
	tick  func(context.Context)

	mu       sync.Mutex
	interval time.Duration
	changed  chan struct{}
}

// SetTick sets the tick function. Needed when the tick depends on an object
// that itself needs an already created Loop. Call only before Run.
func (l *Loop) SetTick(tick func(context.Context)) {
	l.tick = tick
}

// New creates a loop with the given interval that calls tick on every tick.
func New(log *slog.Logger, token string, interval time.Duration, tick func(context.Context)) *Loop {
	return &Loop{
		log:      log,
		token:    token,
		tick:     tick,
		interval: interval,
		changed:  make(chan struct{}, 1),
	}
}

// SetInterval changes the tick interval without restarting the loop.
func (l *Loop) SetInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	l.mu.Lock()
	if d == l.interval {
		l.mu.Unlock()
		return
	}
	l.interval = d
	l.mu.Unlock()

	select {
	case l.changed <- struct{}{}:
	default:
	}
}

// Interval returns the current tick interval.
func (l *Loop) Interval() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.interval
}

// NextTick returns the moment of the next tick after now: the nearest second
// that falls on the agent's offset within the current interval.
func (l *Loop) NextTick(now time.Time) time.Time {
	interval := l.Interval()
	sec := int64(interval / time.Second)
	if sec <= 0 {
		return now
	}
	offset := int64(Offset(l.token, interval) / time.Second)
	delta := offset - now.Unix()%sec
	if delta <= 0 {
		delta += sec
	}
	return now.Truncate(time.Second).Add(time.Duration(delta) * time.Second)
}

// Run runs the loop until ctx is cancelled. Returns nil on a normal shutdown.
func (l *Loop) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		next := l.NextTick(time.Now())
		timer.Reset(time.Until(next))
		l.log.Debug("next tick", "at", next.Format(time.RFC3339))

		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil
		case <-l.changed:
			// The interval changed, recompute the tick moment.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			l.tick(ctx)
		}
	}
}
