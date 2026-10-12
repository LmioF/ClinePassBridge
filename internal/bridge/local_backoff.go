package bridge

import (
	"errors"
	"time"
)

const local429Delay = 5 * time.Second
const local429ContextPoll = 100 * time.Millisecond

type backoffClock struct {
	now   func() time.Time
	timer func(time.Duration) (<-chan time.Time, func())
}

func realBackoffTimer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

func (s *Service) backoffClock() backoffClock {
	if s.localBackoffClock != nil {
		return *s.localBackoffClock
	}
	return backoffClock{time.Now, realBackoffTimer}
}

// Local admission errors must opt in. Status/text alone cannot distinguish an
// upstream 429 from quota, egress or team admission rejected by this plugin.
func isLocal429(err error) bool {
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != 429 {
		return false
	}
	var marked interface{ Local429() bool }
	if errors.As(err, &marked) && marked.Local429() {
		return true
	}
	var upstream *upstreamError
	return errors.As(err, &upstream) && upstream.Local
}

type hostRequestContext struct {
	Supported bool       `json:"supported"`
	State     string     `json:"state"`
	Deadline  *time.Time `json:"deadline,omitempty"`
}

type hostContextResult struct {
	state hostRequestContext
	err   error
}

// A callback is queried asynchronously so a stalled/older host cannot hold the
// local timer or plugin shutdown. There is at most one pending query per wait.
func (s *Service) queryRequestContext(callbackID string) <-chan hostContextResult {
	done := make(chan hostContextResult, 1)
	go func() {
		var state hostRequestContext
		err := s.call("host.request.context", map[string]any{"host_callback_id": callbackID}, &state)
		done <- hostContextResult{state, err}
	}()
	return done
}

func requestContextError(state hostRequestContext) error {
	switch state.State {
	case "canceled", "closed":
		return fail(499, "client request canceled during local backoff")
	case "deadline_exceeded":
		return fail(504, "request deadline exceeded during local backoff")
	}
	return nil
}

// Called exactly once at the RPC boundary, after executor leases/slots and
// upstream streams have been released. Keep the original error and retry data.
func (s *Service) applyLocal429Backoff(r ExecutorRequest, err error) error {
	if !isLocal429(err) {
		return err
	}
	if beginErr := s.begin(); beginErr != nil {
		return beginErr
	}
	defer s.active.Done()
	s.mu.RLock()
	stopCh := s.stopCh
	s.mu.RUnlock()
	clock := s.backoffClock()
	if r.deadline.IsZero() {
		r.deadline = clock.now().Add(time.Duration(s.config().TimeoutSeconds) * time.Second)
	}
	if !clock.now().Before(r.deadline) {
		return fail(504, "request deadline exceeded during local backoff")
	}
	finished, stopFinished := clock.timer(local429Delay)
	defer stopFinished()
	expired, stopExpired := clock.timer(r.deadline.Sub(clock.now()))
	defer func() { stopExpired() }()
	var poll <-chan time.Time
	stopPoll := func() {}
	defer func() { stopPoll() }()
	var query <-chan hostContextResult
	if r.HostCallbackID != "" {
		query = s.queryRequestContext(r.HostCallbackID)
	}
	for {
		select {
		case <-stopCh:
			return fail(503, "ClinePassBridge is shutting down")
		case <-expired:
			return fail(504, "request deadline exceeded during local backoff")
		case <-finished:
			// Give a ready cancellation/deadline priority over returning 429.
			select {
			case <-stopCh:
				return fail(503, "ClinePassBridge is shutting down")
			default:
			}
			if !clock.now().Before(r.deadline) {
				return fail(504, "request deadline exceeded during local backoff")
			}
			select {
			case result := <-query:
				if result.err == nil && result.state.Supported {
					if contextErr := requestContextError(result.state); contextErr != nil {
						return contextErr
					}
				}
			default:
			}
			return err
		case result := <-query:
			query = nil
			// Original CPA versions do not implement this callback. Continue with
			// local deadline/stop observation and report that limitation to UI.
			if result.err != nil || !result.state.Supported {
				continue
			}
			if contextErr := requestContextError(result.state); contextErr != nil {
				return contextErr
			}
			if result.state.Deadline != nil && result.state.Deadline.Before(r.deadline) {
				r.deadline = *result.state.Deadline
				if !clock.now().Before(r.deadline) {
					return fail(504, "request deadline exceeded during local backoff")
				}
				stopExpired()
				expired, stopExpired = clock.timer(r.deadline.Sub(clock.now()))
			}
			poll, stopPoll = clock.timer(local429ContextPoll)
		case <-poll:
			stopPoll()
			poll = nil
			query = s.queryRequestContext(r.HostCallbackID)
		}
	}
}

// LocalBackoffCapabilities is a bounded optional probe for management/UI code.
// Unknown hosts remain usable and explicitly report no cancellation observer.
type LocalBackoffCapabilities struct {
	PluginDelayMS           int64 `json:"plugin_delay_ms"`
	RequestCancelObservable bool  `json:"request_cancel_observable"`
	HostSelectionDelay      bool  `json:"host_selection_delay"`
}

func (s *Service) LocalBackoffCapabilities() LocalBackoffCapabilities {
	result := LocalBackoffCapabilities{PluginDelayMS: local429Delay.Milliseconds()}
	type hostCapabilities struct {
		RequestContext      bool `json:"request_context_supported"`
		ClineSelectionDelay bool `json:"local_delay_supported"`
	}
	done := make(chan hostCapabilities, 1)
	go func() {
		var caps hostCapabilities
		if s.call("host.scheduling.capabilities", map[string]any{}, &caps) == nil {
			done <- caps
		} else {
			done <- hostCapabilities{}
		}
	}()
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	s.mu.RLock()
	stopCh := s.stopCh
	s.mu.RUnlock()
	select {
	case caps := <-done:
		result.RequestCancelObservable = caps.RequestContext
		result.HostSelectionDelay = caps.ClineSelectionDelay
	case <-timer.C:
	case <-stopCh:
	}
	return result
}
