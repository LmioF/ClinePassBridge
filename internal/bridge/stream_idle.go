package bridge

import (
	"sync"
	"time"
)

// One goroutine owns the timer. Activity changes the deadline under the same
// lock used to check expiry, so a stale timer tick cannot cut off fresh output.
type streamReadWatchdog struct {
	mu           sync.Mutex
	deadline     time.Time
	idleTimeout  time.Duration
	lastOutput   time.Time
	downstream   bool
	emitElapsed  time.Duration
	diagnostics  *StreamDiagnostics
	debugEnabled func() bool
	err          error
	stopped      bool
	wake         chan struct{}
	stopCh       chan struct{}
	shutdown     <-chan struct{}
	done         chan struct{}
	closeOnce    sync.Once
	closeStream  func()
	closeOutput  func(error)
	parentError  func() error
}

func (s *Service) newStreamReadWatchdog(up upstreamStream, idleTimeout time.Duration) *streamReadWatchdog {
	deadline := up.deadline
	if deadline.IsZero() {
		deadline = time.Now().Add(time.Duration(s.config().TimeoutSeconds) * time.Second)
	}
	w := &streamReadWatchdog{
		deadline: deadline, idleTimeout: idleTimeout,
		debugEnabled: s.debugEnabled,
		wake:         make(chan struct{}, 1), stopCh: make(chan struct{}), done: make(chan struct{}),
		shutdown:    s.stopCh,
		closeStream: func() { s.closeUpstream(up.StreamID) },
	}
	if up.output != nil {
		w.closeOutput = func(err error) { up.output.close(err, up.output.phase()) }
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

// Only the stream consumer touches diagnostics. Once disabled, this request
// stops collecting even if debug is enabled again before it completes.
func (w *streamReadWatchdog) streamDiagnostics() *StreamDiagnostics {
	if w.diagnostics != nil && !w.debugEnabled() {
		w.diagnostics = nil
	}
	return w.diagnostics
}

func (w *streamReadWatchdog) expired(now time.Time) error {
	select {
	case <-w.shutdown:
		return bridgeUnavailable("ClinePassBridge is shutting down")
	default:
	}
	if w.parentError != nil {
		if err := w.parentError(); err != nil {
			return err
		}
	}
	if !now.Before(w.deadline) {
		return requestTimedOut("Cline upstream request timed out")
	}
	if !w.downstream && w.idleTimeout > 0 && !w.lastOutput.IsZero() && !now.Before(w.lastOutput.Add(w.idleTimeout)) {
		return streamIdleTimeout(w.idleTimeout)
	}
	return nil
}

// Downstream backpressure is not upstream inactivity. Preserve the already-used
// idle budget while suspending it, and keep the absolute deadline active.
func (w *streamReadWatchdog) emit(fn func() error) (err error) {
	w.mu.Lock()
	if w.err == nil {
		w.err = w.expired(time.Now())
	}
	if w.err != nil {
		err = w.err
		w.mu.Unlock()
		return err
	}
	started := time.Now()
	w.downstream = true
	w.mu.Unlock()
	w.notify()
	defer func() {
		w.mu.Lock()
		elapsed := time.Since(started)
		if !w.lastOutput.IsZero() {
			w.lastOutput = w.lastOutput.Add(elapsed)
		}
		w.downstream = false
		if w.err == nil {
			w.err = w.expired(time.Now())
		}
		if err == nil {
			err = w.err
		}
		w.mu.Unlock()
		if d := w.streamDiagnostics(); d != nil {
			w.emitElapsed += elapsed
			d.DownstreamEmitCount++
			d.DownstreamEmitMS = w.emitElapsed.Milliseconds()
			if elapsed.Milliseconds() > d.DownstreamEmitMaxMS {
				d.DownstreamEmitMaxMS = elapsed.Milliseconds()
			}
		}
		w.notify()
	}()
	return fn()
}

func (w *streamReadWatchdog) notify() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
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
	w.notify()
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
			failure := w.err
			w.mu.Unlock()
			// Unblock a backpressured emit before joining upstream cleanup. The
			// final consumer close shares the same Once and cannot close twice.
			if w.closeOutput != nil {
				w.closeOutput(failure)
			}
			w.close()
			return
		}
		next := w.deadline
		if !w.downstream && w.idleTimeout > 0 && !w.lastOutput.IsZero() {
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
		case <-w.shutdown:
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
