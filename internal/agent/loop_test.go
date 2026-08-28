package agent_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"metrics-agent/internal/agent"
	"metrics-agent/internal/logging"
)

const (
	tokenA = "9f2c1f8a-1111-4222-8333-444455556666"
	tokenB = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
)

func quiet() *slog.Logger { return logging.New("none", io.Discard) }

func TestOffsetDeterministic(t *testing.T) {
	first := agent.Offset(tokenA, agent.DefaultReportInterval)
	if second := agent.Offset(tokenA, agent.DefaultReportInterval); first != second {
		t.Fatalf("the same token gave different offsets: %v and %v", first, second)
	}
	if other := agent.Offset(tokenB, agent.DefaultReportInterval); other == first {
		t.Fatalf("different tokens gave the same offset: %v", first)
	}
	if first < 0 || first >= agent.DefaultReportInterval {
		t.Fatalf("offset out of interval: %v", first)
	}
}

func TestNextTickHitsOffset(t *testing.T) {
	l := agent.New(quiet(), tokenA, agent.DefaultReportInterval, func(context.Context) {})
	offset := int64(agent.Offset(tokenA, agent.DefaultReportInterval) / time.Second)

	now := time.Now()
	for i := 0; i < 120; i++ {
		next := l.NextTick(now.Add(time.Duration(i) * time.Second))
		if next.Unix()%60 != offset {
			t.Fatalf("tick did not land on offset %d: %v", offset, next)
		}
		if d := next.Sub(now.Add(time.Duration(i) * time.Second)); d <= 0 || d > agent.DefaultReportInterval {
			t.Fatalf("tick outside the expected interval: %v", d)
		}
	}
}

func TestTicksAtInterval(t *testing.T) {
	ticks := make(chan time.Time, 4)
	l := agent.New(quiet(), tokenA, time.Second, func(context.Context) {
		select {
		case ticks <- time.Now():
		default:
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()

	var prev time.Time
	for i := 0; i < 3; i++ {
		select {
		case at := <-ticks:
			if !prev.IsZero() {
				if d := at.Sub(prev); d < 900*time.Millisecond || d > 1500*time.Millisecond {
					t.Fatalf("tick period %v, want ~1s", d)
				}
			}
			prev = at
		case <-time.After(3 * time.Second):
			t.Fatal("tick did not arrive")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}
}

func TestSetIntervalAppliedWithoutRestart(t *testing.T) {
	ticks := make(chan struct{}, 1)
	l := agent.New(quiet(), tokenA, time.Hour, func(context.Context) {
		select {
		case ticks <- struct{}{}:
		default:
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)

	// The first interval is an hour, there must be no tick.
	select {
	case <-ticks:
		t.Fatal("tick arrived with an hour interval")
	case <-time.After(200 * time.Millisecond):
	}

	l.SetInterval(time.Second)
	if got := l.Interval(); got != time.Second {
		t.Fatalf("Interval() = %v", got)
	}
	select {
	case <-ticks:
	case <-time.After(3 * time.Second):
		t.Fatal("no tick after SetInterval: the loop did not pick up the change")
	}
}

func TestRunStopsFastOnCancel(t *testing.T) {
	l := agent.New(quiet(), tokenA, agent.DefaultReportInterval, func(context.Context) {})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned an error: %v", err)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("shutdown took %v, want <= 1s", d)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not finish within 1 second")
	}
}
