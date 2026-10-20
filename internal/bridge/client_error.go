package bridge

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ClientErrorMessage applies the current compatibility setting at the client
// boundary; diagnostic logs keep the original error.
func (s *Service) ClientErrorMessage(err error) string {
	if s != nil && !s.config().ClientRetryCompatibility {
		return safeError(err)
	}
	return ClientErrorMessage(err)
}

// ClientErrorMessage separates a known transport fault from nested provider
// exception codes. Text-classifying clients otherwise mistake the internal
// TransferEncodingError's 400 for a rejected request, even after an HTTP 502.
// The original exception remains on the error and in the diagnostic log.
func ClientErrorMessage(err error) string {
	var upstream *upstreamError
	if errors.As(err, &upstream) && upstream.Kind == transportIncompleteKind && upstream.IsRequestScoped() {
		return "HTTP 502: upstream response stream ended before completion: unexpected EOF (upstream_transport_incomplete)"
	}
	if errors.As(err, &upstream) && upstream.IsRequestScoped() {
		switch upstream.Kind {
		case upstreamServiceKind:
			return fmt.Sprintf("HTTP %d: upstream service failed; retry the request (%s)", upstream.Status, upstream.Kind)
		case upstreamProtocolKind:
			return "HTTP 502: upstream returned an incomplete or invalid response (upstream_protocol_error)"
		case upstreamNetworkKind, upstreamCallbackKind:
			return "HTTP 502: upstream connection interrupted (" + upstream.Kind + ")"
		}
	}
	return safeError(err)
}

// HostErrorDetails preserves stock CPA's transport classification across its
// stream and RPC ABIs. This is independent of client retry normalization:
// disabling it keeps diagnostic text where the host can safely classify it.
// Account/model protections win; ordinary and unknown failures are exempt by
// default. Deterministic request faults stop fallback across both error ABIs.
// A zero status is an absent RPC status, not an HTTP response status: stock CPA
// recognizes the transport message before rendering a normal 500 client error.
func (s *Service) HostErrorDetails(err error) (string, int) {
	return s.hostErrorDetails(err, false)
}

func hostErrorStatus(err error) int {
	var coded interface{ StatusCode() int }
	if errors.As(err, &coded) && coded.StatusCode() >= 400 && coded.StatusCode() <= 599 {
		return coded.StatusCode()
	}
	return http.StatusInternalServerError
}

func (s *Service) hostErrorDetails(err error, stream bool) (string, int) {
	message := s.ClientErrorMessage(err)
	status := hostErrorStatus(err)
	var upstream *upstreamError
	_ = errors.As(err, &upstream)
	kind, _ := faultIdentity(err)
	policy := s.hostFaultPolicy(err)
	if err == nil {
		return message, status
	}
	if policy.Explicit && !stream {
		// Keep the wire body/status matchable so CPA also applies this action
		// to its current-request retry loop and force-cooldown handling.
		return safeError(err), status
	}
	if policy.Cooldown {
		// A structured invalid_grant/account error may arrive as HTTP 400.
		// Preserve that diagnostic status on the error, while avoiding the
		// stock host's blanket request-fault interpretation of a wire 400.
		if kind == "upstream_auth_error" && status != 401 && status != 402 && status != 403 && status != 429 {
			status = http.StatusUnauthorized
		}
		if kind == upstreamModelKind && !policy.Explicit {
			return protectedHostMessage(&upstreamError{APIError: &APIError{Status: status, Kind: kind}}, message), status
		}
		if upstream != nil && !policy.Explicit {
			return protectedHostMessage(upstream, message), status
		}
		if containsHostTransportMarker(message) || strings.HasPrefix(strings.TrimSpace(message), "{") {
			message = fmt.Sprintf("HTTP %d: credential request failed", status)
		}
		return message, status
	}
	if kind == countTokensUnavailableKind && !policy.Explicit {
		return message, status
	}
	if policy.Stop {
		if policy.Explicit {
			message = "Plugin error rule stops this request"
			for _, marker := range []string{rateLimitMarker, quotaBlockMarker, "[clinepassbridge:egress_unavailable]", "[clinepassbridge:ip_rate_limit]"} {
				if strings.Contains(safeError(err), marker) {
					message = marker + " " + message
				}
			}
		}
		switch kind {
		case requestCanceledKind:
			return "context canceled", 0
		case requestDeadlineKind:
			return "context deadline exceeded", 0
		case responseLimitKind:
			return responseLimitHostMessage(safeError(err)), 413
		}
		wireStatus := status
		if status == 402 || status == 429 {
			wireStatus = 0
		}
		return requestFaultHostMessage(status, "invalid_request_error", message), wireStatus
	}
	var marker string
	if policy.Explicit {
		// A custom continue action must not inherit request/model JSON or
		// text-based model-support classification from the original failure.
		message = fmt.Sprintf("HTTP %d: plugin error rule permits route fallback", status)
	}
	switch kind {
	case transportIncompleteKind:
		marker = "unexpected EOF"
	case upstreamStreamIdleTimeoutKind:
		marker = "i/o timeout"
	case upstreamNetworkKind, upstreamCallbackKind:
		marker = "[clinepassbridge:cpa_compat:connection aborted]"
	default:
		marker = "[clinepassbridge:cpa_compat:server misbehaving]"
	}
	if !strings.Contains(strings.ToLower(message), strings.ToLower(marker)) {
		message += ": " + marker
	}
	return message, 0
}
