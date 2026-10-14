package bridge

import (
	"sync"
	"time"
)

// One goroutine owns the timer. Activity changes the deadline under the same
// lock used to check expiry, so a stale timer tick cannot cut off fresh output.
type streamReadWatchdog struct {
	mu          sync.Mutex
	deadline    time.Time
	idleTimeout time.Duration
	lastOutput  time.Time
	err         error
	stopped     bool
	wake        chan struct{}
	stopCh      chan struct{}
	done        chan struct{}
	closeOnce   sync.Once
	closeStream func()
	parentError func() error
}

func (s *Service) newStreamReadWatchdog(up upstreamStream, idleTimeout time.Duration) *streamReadWatchdog {
	deadline := up.deadline
	if deadline.IsZero() {
		deadline = time.Now().Add(time.Duration(s.config().TimeoutSeconds) * time.Second)
	}
	w := &streamReadWatchdog{
		deadline: deadline, idleTimeout: idleTimeout,
		wake: make(chan struct{}, 1), stopCh: make(chan struct{}), done: make(chan struct{}),
		closeStream: func() { s.closeUpstream(up.StreamID) },
	}
	if up.local != nil {
		w.parentError = func() error {
			if up.local.op.ctx.Err() != nil {
				return egressContextError(up.local.op.ctx)
			}
			return nil
		}
	}
	return w
}

func (w *streamReadWatchdog) expired(now time.Time) error {
	if w.parentError != nil {
		if err := w.parentError(); err != nil {
			return err
		}
	}
	if !now.Before(w.deadline) {
		return requestTimedOut("Cline upstream request timed out")
	}
	if w.idleTimeout > 0 && !w.lastOutput.IsZero() && !now.Before(w.lastOutput.Add(w.idleTimeout)) {
		return streamIdleTimeout(w.idleTimeout)
	}
	return nil
}

func (w *streamReadWatchdog) effectiveOutput() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped || w.err != nil {
		return w.err
	}
	now := time.Now()
	w.err = w.expired(now)
	if w.err == nil && w.idleTimeout > 0 {
		w.lastOutput = now
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return w.err
}

func (w *streamReadWatchdog) run() {
	defer close(w.done)
	timer := time.NewTimer(time.Until(w.deadline))
	defer timer.Stop()
	for {
		w.mu.Lock()
		if w.stopped {
			w.mu.Unlock()
			return
		}
		if w.err == nil {
			w.err = w.expired(time.Now())
		}
		if w.err != nil {
			w.mu.Unlock()
			w.close()
			return
		}
		next := w.deadline
		if w.idleTimeout > 0 && !w.lastOutput.IsZero() {
			if idleDeadline := w.lastOutput.Add(w.idleTimeout); idleDeadline.Before(next) {
				next = idleDeadline
			}
		}
		w.mu.Unlock()
		timer.Reset(time.Until(next))
		select {
		case <-timer.C:
		case <-w.wake:
		case <-w.stopCh:
			return
		}
	}
}

func (w *streamReadWatchdog) failure() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *streamReadWatchdog) close() { w.closeOnce.Do(w.closeStream) }

func (w *streamReadWatchdog) stop() {
	w.mu.Lock()
	if !w.stopped {
		w.stopped = true
		close(w.stopCh)
	}
	w.mu.Unlock()
	// Join any in-flight close before returning or reusing a stream identifier.
	<-w.done
}
