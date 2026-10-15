package bridge

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	upstreamServiceKind   = "upstream_service_error"
	upstreamProtocolKind  = "upstream_protocol_error"
	upstreamCallbackKind  = "upstream_callback_error"
	responseLimitKind     = "response_limit_exceeded"
	bridgeUnavailableKind = "bridge_unavailable"
)

// Classification follows the failure's source, not a list of provider messages.
// Local is deliberately not set: a local limit may occur AFTER an upstream call.
func requestFault(status int, kind, message string) error {
	return &upstreamError{APIError: &APIError{Status: status, Kind: kind, Message: message}, Scope: "request"}
}

func upstreamProtocolFailure(message string) error {
	return requestFault(502, upstreamProtocolKind, message)
}

func responseLimitFailure(message string) error {
	return requestFault(413, responseLimitKind, message)
}

func bridgeUnavailable(message string) error {
	return requestFault(503, bridgeUnavailableKind, message)
}

// Stock CPA loses status/type information at stream.close and then matches
// these phrases anywhere in the error. Account faults containing such phrases
// must not accidentally acquire a transport exemption. Raw details stay in logs.
func containsHostTransportMarker(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "context canceled" || lower == "context deadline exceeded" || lower == "eof" {
		return true
	}
	for _, marker := range []string{
		"unexpected eof", "i/o timeout", "connection reset", "connection refused",
		"connection aborted", "broken pipe", "use of closed network connection",
		"tls: tls handshake", "tls handshake timeout", "wsarecv", "wsasend",
		"a connection attempt failed", "no such host", "server misbehaving",
		"network is unreachable", "no route to host", "websocket: close 1000",
		"websocket: close 1001", "websocket: close 1006",
		"http2: client connection force closed via clientconn.close", "http2: client conn is closed",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func protectedHostMessage(err *upstreamError, message string) string {
	if !containsHostTransportMarker(message) {
		return message
	}
	message = fmt.Sprintf("HTTP %d: upstream request failed (%s)", err.Status, err.Kind)
	if err.Kind == "upstream_team_rate_limited" {
		message = rateLimitMarker + " " + message
	}
	if !err.RetryAt.IsZero() {
		message += fmt.Sprintf(". Retry after %ds.", retrySeconds(err.RetryAt, time.Now()))
	}
	return message
}

// A deterministic local size limit should stop same-request credential rotation,
// unlike transient service failures. Stock CPA understands this JSON error type.
func responseLimitHostMessage(message string) string {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": "HTTP 413: " + message,
		"type":    "invalid_request_error", "code": "message_too_big",
	}})
	return string(body)
}
