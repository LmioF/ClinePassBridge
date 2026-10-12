package bridge

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type manualBackoffTimer struct {
	d       time.Duration
	c       chan time.Time
	stopped bool
}

type manualBackoffClock struct {
	mu      sync.Mutex
	at      time.Time
	created chan *manualBackoffTimer
	all     []*manualBackoffTimer
}

func newManualBackoffClock(s *Service) *manualBackoffClock {
	c := &manualBackoffClock{at: time.Now(), created: make(chan *manualBackoffTimer, 20)}
	s.localBackoffClock = &backoffClock{
		now: func() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.at },
		timer: func(d time.Duration) (<-chan time.Time, func()) {
			t := &manualBackoffTimer{d: d, c: make(chan time.Time, 1)}
			c.mu.Lock()
			c.all = append(c.all, t)
			c.mu.Unlock()
			c.created <- t
			return t.c, func() { c.mu.Lock(); t.stopped = true; c.mu.Unlock() }
		},
	}
	return c
}

func (c *manualBackoffClock) next(t *testing.T) *manualBackoffTimer {
	t.Helper()
	select {
	case timer := <-c.created:
		return timer
	case <-time.After(time.Second):
		t.Fatal("backoff timer not created")
		return nil
	}
}

func (c *manualBackoffClock) fire(timer *manualBackoffTimer, advance time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(advance)
	now := c.at
	c.mu.Unlock()
	timer.c <- now
}

func receiveBackoff(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		t.Fatal("backoff did not return")
		return nil
	}
}

type markedLocal429 struct{ *APIError }

func (e *markedLocal429) Local429() bool { return true }

func TestLocalBackoffScope(t *testing.T) {
	for _, scope := range []string{"quota", "egress", "team"} {
		err := &markedLocal429{&APIError{429, "local_" + scope, "local admission"}}
		if !isLocal429(fmt.Errorf("wrapped: %w", err)) {
			t.Fatalf("missed %s local 429", scope)
		}
	}
	for _, err := range []error{nil, fail(429, "local quota text from upstream"), &markedLocal429{&APIError{503, "local", "busy"}}, &upstreamError{APIError: &APIError{429, "upstream_team_rate_limited", "team limit"}}} {
		s := NewService()
		clock := newManualBackoffClock(s)
		if got := s.applyLocal429Backoff(ExecutorRequest{}, err); got != err {
			t.Fatalf("changed out-of-scope error %v", err)
		}
		if len(clock.all) != 0 {
			t.Fatal("out-of-scope error was delayed")
		}
	}
}

func TestLocalBackoffFiveSecondsPreservesError(t *testing.T) {
	s := NewService()
	clock := newManualBackoffClock(s)
	err := &upstreamError{APIError: &APIError{429, "local_team", "retry"}, Local: true, RetryAt: clock.at.Add(time.Minute)}
	done := make(chan error, 1)
	go func() { done <- s.applyLocal429Backoff(ExecutorRequest{deadline: clock.at.Add(20 * time.Second)}, err) }()
	finished, deadline := clock.next(t), clock.next(t)
	if finished.d != 5*time.Second || deadline.d != 20*time.Second {
		t.Fatalf("timers %v %v", finished.d, deadline.d)
	}
	select {
	case <-done:
		t.Fatal("returned before 5 seconds")
	default:
	}
	clock.fire(finished, 5*time.Second)
	if got := receiveBackoff(t, done); got != err {
		t.Fatalf("retry error changed: %v", got)
	}
	clock.mu.Lock()
	defer clock.mu.Unlock()
	if !finished.stopped || !deadline.stopped || len(clock.all) != 2 {
		t.Fatal("timers leaked or delay repeated")
	}
}

func TestLocalBackoffDeadlineAndStop(t *testing.T) {
	for _, mode := range []string{"expired", "deadline", "stop"} {
		t.Run(mode, func(t *testing.T) {
			s := registeredService(t, "")
			clock := newManualBackoffClock(s)
			r := ExecutorRequest{deadline: clock.at.Add(time.Second)}
			if mode == "expired" {
				r.deadline = clock.at
			}
			err := &markedLocal429{&APIError{429, "local_quota", "wait"}}
			done := make(chan error, 1)
			go func() { done <- s.applyLocal429Backoff(r, err) }()
			if mode == "expired" {
				if statusOf(receiveBackoff(t, done)) != 504 || len(clock.all) != 0 {
					t.Fatal("expired request waited")
				}
				return
			}
			clock.next(t)
			deadline := clock.next(t)
			want := 504
			if mode == "deadline" {
				clock.fire(deadline, time.Second)
			} else {
				want = 503
				shutdown := make(chan error, 1)
				go func() { _, e := s.Handle("plugin.quiesce", nil); shutdown <- e }()
				if receiveBackoff(t, shutdown) != nil {
					t.Fatal("quiesce failed")
				}
			}
			if got := receiveBackoff(t, done); statusOf(got) != want {
				t.Fatalf("status = %v want %d", got, want)
			}
		})
	}
}

func TestLocalBackoffObservesHostCancelAndEarlierDeadline(t *testing.T) {
	for _, mode := range []string{"canceled", "deadline_exceeded", "earlier_deadline", "closed"} {
		t.Run(mode, func(t *testing.T) {
			s := NewService()
			clock := newManualBackoffClock(s)
			calls := 0
			s.SetHost(func(method string, in, out any) error {
				if method != "host.request.context" || in.(map[string]any)["host_callback_id"] != "callback" {
					return errors.New("unexpected callback")
				}
				calls++
				state := hostRequestContext{Supported: true, State: "active"}
				if mode == "earlier_deadline" {
					early := clock.at.Add(2 * time.Second)
					state.Deadline = &early
				} else if calls > 1 {
					state.State = mode
				}
				*out.(*hostRequestContext) = state
				return nil
			})
			done := make(chan error, 1)
			go func() {
				done <- s.applyLocal429Backoff(ExecutorRequest{HostCallbackID: "callback", deadline: clock.at.Add(20 * time.Second)}, &markedLocal429{&APIError{429, "local_egress", "wait"}})
			}()
			clock.next(t)
			clock.next(t)
			want := 499
			if mode == "earlier_deadline" {
				early := clock.next(t)
				if early.d != 2*time.Second {
					t.Fatalf("host deadline timer = %v", early.d)
				}
				clock.next(t)
				clock.fire(early, 2*time.Second)
				want = 504
			} else {
				poll := clock.next(t)
				if poll.d != 100*time.Millisecond {
					t.Fatalf("poll = %v", poll.d)
				}
				clock.fire(poll, 100*time.Millisecond)
				if mode == "deadline_exceeded" {
					want = 504
				}
			}
			if got := receiveBackoff(t, done); statusOf(got) != want {
				t.Fatalf("got %v want %d", got, want)
			}
		})
	}
}

func TestLocalBackoffPendingHostDoesNotHoldStop(t *testing.T) {
	s := registeredService(t, "")
	clock := newManualBackoffClock(s)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	s.SetHost(func(method string, in, out any) error {
		close(entered)
		<-release
		return errors.New("host unavailable")
	})
	done := make(chan error, 1)
	go func() {
		done <- s.applyLocal429Backoff(ExecutorRequest{HostCallbackID: "callback", deadline: clock.at.Add(time.Minute)}, &markedLocal429{&APIError{429, "local", "wait"}})
	}()
	clock.next(t)
	clock.next(t)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("callback not entered")
	}
	shutdown := make(chan error, 1)
	go func() { _, err := s.Handle("plugin.shutdown", nil); shutdown <- err }()
	if got := receiveBackoff(t, done); statusOf(got) != 503 {
		t.Fatalf("stop = %v", got)
	}
	if receiveBackoff(t, shutdown) != nil {
		t.Fatal("shutdown did not finish")
	}
}

func TestLocalBackoffHandleLocalTeamWithoutUpstream(t *testing.T) {
	for _, method := range []string{"executor.execute", "executor.execute_stream"} {
		t.Run(method, func(t *testing.T) {
			s := registeredService(t, "")
			clock := newManualBackoffClock(s)
			host := newFakeHost()
			s.SetHost(host.call)
			c := Credential{Type: Provider, ID: "credential-1", Label: "test credential", APIKey: "test-key"}
			lease, err := s.rateLimits.acquire(c, "cline-pass/deepseek-v4.1-flash")
			if err != nil {
				t.Fatal(err)
			}
			lease.finish(&upstreamError{APIError: &APIError{429, "upstream_team_rate_limited", "team"}, RetryAt: time.Now().Add(time.Minute), Scope: "upstream_team"})
			done := make(chan error, 1)
			go func() { _, e := s.Handle(method, executorRequest("stream")); done <- e }()
			finished := clock.next(t)
			clock.next(t)
			if finished.d != local429Delay {
				t.Fatalf("delay = %v", finished.d)
			}
			s.rateLimits.mu.Lock()
			for _, window := range s.rateLimits.windows {
				if window.probe != nil {
					t.Error("local wait holds upstream probe")
				}
			}
			s.rateLimits.mu.Unlock()
			clock.fire(finished, local429Delay)
			if got := receiveBackoff(t, done); !isLocal429(got) {
				t.Fatalf("got %v", got)
			}
			host.mu.Lock()
			defer host.mu.Unlock()
			if len(host.opened) != 0 || len(host.emitted) != 0 {
				t.Fatal("local wait opened upstream or emitted streaming data")
			}
		})
	}
}

func TestLocalBackoffCapabilitiesOfficialHostFallback(t *testing.T) {
	s := NewService()
	s.SetHost(func(method string, in, out any) error { return errors.New("unsupported callback") })
	got := s.LocalBackoffCapabilities()
	if got.PluginDelayMS != 5000 || got.RequestCancelObservable || got.HostSelectionDelay {
		t.Fatalf("fallback = %+v", got)
	}
}
