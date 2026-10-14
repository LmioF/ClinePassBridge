package bridge

import (
	"errors"
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

func (s *Service) closeOutputStream(streamID string, entry *LogEntry, err error) {
	if streamID == "" {
		return
	}
	payload := map[string]any{"stream_id": streamID, "error": s.ClientErrorMessage(err)}
	if err != nil {
		details := streamErrorDetails{HTTPStatus: statusOf(err), Code: "upstream_error", Phase: entry.ErrorPhase}
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
	entry.StreamErrorDetailsSupported = new(bool)
	*entry.StreamErrorDetailsSupported = closeErr == nil && response.ErrorDetailsSupported
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

// A truncated or idle provider response does not prove team-limit recovery and must
// not add the ordinary failed-probe five-second retreat. Retain any existing
// window, release its probe slot, and leave its original deadline unchanged.
func finishRequestRateLease(lease *rateLease, err error) {
	var upstream *upstreamError
	if lease == nil || !errors.As(err, &upstream) || (upstream.Kind != transportIncompleteKind && upstream.Kind != upstreamStreamIdleTimeoutKind) {
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
