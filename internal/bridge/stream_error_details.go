package bridge

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// The legacy error string remains mandatory for older JSON ABI hosts. New
// hosts acknowledge this additional object without changing the old envelope.
type streamErrorDetails struct {
	HTTPStatus    int        `json:"http_status"`
	Code          string     `json:"code"`
	Scope         string     `json:"scope"`
	RetryAt       *time.Time `json:"retry_at,omitempty"`
	RequestScoped bool       `json:"request_scoped"`
	Phase         string     `json:"phase"`
}

type streamCloseResponse struct {
	ErrorDetailsSupported bool `json:"error_details_supported"`
}

// A watchdog may close the downstream while the consumer is blocked in emit.
// Keep the callback and its receipt here; only the consumer writes LogEntry.
type outputStreamLifecycle struct {
	service   *Service
	id        string
	started   atomic.Bool
	once      sync.Once
	cause     error
	supported bool
}

func (o *outputStreamLifecycle) phase() string {
	if o.started.Load() {
		return "after_output"
	}
	return "before_output"
}

func (o *outputStreamLifecycle) close(err error, phase string) error {
	o.once.Do(func() {
		o.cause = err
		o.supported = o.service.sendOutputStreamClose(o.id, phase, err)
	})
	// Once also joins an in-flight watchdog close before its receipt is read.
	return o.cause
}

func (s *Service) closeOutputStream(streamID string, entry *LogEntry, err error) {
	if streamID == "" {
		return
	}
	recordOutputStreamClose(entry, err, s.sendOutputStreamClose(streamID, entry.ErrorPhase, err))
}

func (s *Service) sendOutputStreamClose(streamID, phase string, err error) bool {
	message, _ := s.HostErrorDetails(err)
	payload := map[string]any{"stream_id": streamID, "error": message}
	if err != nil {
		details := streamErrorDetails{HTTPStatus: statusOf(err), Code: "upstream_error", Phase: phase}
		var coded interface{ Code() string }
		if errors.As(err, &coded) && coded.Code() != "" {
			details.Code = coded.Code()
		}
		var upstream *upstreamError
		if errors.As(err, &upstream) {
			details.Scope = upstream.Scope
			details.RequestScoped = upstream.IsRequestScoped()
			if !upstream.RetryAt.IsZero() {
				at := upstream.RetryAt.UTC()
				details.RetryAt = &at
			}
		}
		payload["error_details"] = details
	}
	var response streamCloseResponse
	closeErr := s.call("host.stream.close", payload, &response)
	return closeErr == nil && response.ErrorDetailsSupported
}

func recordOutputStreamClose(entry *LogEntry, err error, supported bool) {
	entry.StreamErrorDetailsSupported = new(bool)
	*entry.StreamErrorDetailsSupported = supported
	if err != nil {
		if !*entry.StreamErrorDetailsSupported {
			entry.CooldownDisposition = "legacy_host_unknown"
		} else {
			entry.CooldownDisposition = "host_policy"
			var upstream *upstreamError
			if errors.As(err, &upstream) && upstream.IsRequestScoped() {
				entry.CooldownDisposition = "exempt"
			}
		}
	}
}

// A request/service failure does not prove team-limit recovery and must
// not add the ordinary failed-probe five-second retreat. Retain any existing
// window, release its probe slot, and leave its original deadline unchanged.
func finishRequestRateLease(lease *rateLease, err error) {
	var upstream *upstreamError
	if lease == nil || !errors.As(err, &upstream) || !upstream.IsRequestScoped() {
		lease.finish(err)
		return
	}
	l := lease.owner
	l.mu.Lock()
	defer l.mu.Unlock()
	state := l.windows[lease.key]
	if state != nil && state == lease.state && state.probe == lease {
		state.probe = nil
	}
}
