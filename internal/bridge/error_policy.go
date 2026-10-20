package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	upstreamServiceKind        = "upstream_service_error"
	upstreamProtocolKind       = "upstream_protocol_error"
	upstreamCallbackKind       = "upstream_callback_error"
	responseLimitKind          = "response_limit_exceeded"
	bridgeUnavailableKind      = "bridge_unavailable"
	countTokensUnavailableKind = "token_count_unavailable"
)

// Decide the fault's owner separately from whether the current request may
// continue on another route. Unknown failures are not evidence of a bad key.
// Credential/model/team protections remain authoritative over generic faults.
type faultPolicy struct {
	Scope    string
	Cooldown bool
	Stop     bool
	Explicit bool
}

func faultIdentity(err error) (kind, scope string) {
	var upstream *upstreamError
	if errors.As(err, &upstream) {
		// Its source has already classified the response structure. A JSON
		// string quoted in error.message is not a second upstream envelope.
		return upstream.Kind, upstream.Scope
	} else {
		var classified interface{ Code() string }
		if errors.As(err, &classified) {
			kind = classified.Code()
		}
	}
	// A raw API/RPC error may still carry stable model/account identifiers.
	// Resolve those before the generic request-status or unknown-fault defaults.
	identity := map[string]any{"error": map[string]any{"code": kind}}
	var body map[string]any
	if err != nil && len(err.Error()) <= 64<<10 {
		var parsed map[string]any
		if json.Unmarshal([]byte(err.Error()), &parsed) == nil && parsed != nil {
			body = parsed
		}
	}
	if isUpstreamCredentialError(identity) || isUpstreamCredentialError(body) {
		return "upstream_auth_error", "account"
	}
	if isUpstreamModelError(identity) || isUpstreamModelError(body) {
		return upstreamModelKind, "credential_model"
	}
	return kind, scope
}

func faultPolicyFor(err error) faultPolicy {
	if err == nil {
		return faultPolicy{}
	}
	status := 0
	var coded interface{ StatusCode() int }
	if errors.As(err, &coded) {
		status = coded.StatusCode()
	}
	kind, scope := faultIdentity(err)
	// These scopes come from local admission or an explicitly recognized IP
	// fault. Their own pool/group window is independent of credential health.
	if scope == "egress_ip" || scope == "egress_pool" {
		return faultPolicy{Scope: scope, Stop: status == 429}
	}
	protected := status == 401 || status == 402 || status == 403 || status == 429
	protected = protected || scope == "account" || scope == "credential_model" || scope == "upstream_team"
	protected = protected || kind == upstreamModelKind || kind == "upstream_auth_error" || kind == "upstream_quota_exhausted"
	if protected {
		if scope == "" || scope == "request" {
			scope = "credential_model"
		}
		return faultPolicy{Scope: scope, Cooldown: true}
	}
	if scope == "" {
		scope = "request"
	}
	stop := kind == upstreamRequestKind || kind == responseLimitKind || kind == countTokensUnavailableKind ||
		kind == requestCanceledKind || kind == requestDeadlineKind
	switch status {
	case 400, 409, 413, 415, 422:
		stop = true
	}
	return faultPolicy{Scope: scope, Stop: stop}
}

// Plugin-owned rules override the baseline before an RPC status or stream
// envelope is normalized. Their ordering and match semantics mirror stock CPA.
// An empty rule list still means inherit the host's configuration; that
// configuration is not exposed by the public plugin callback interface.
func (s *Service) hostFaultPolicy(err error) faultPolicy {
	policy := faultPolicyFor(err)
	if s == nil || err == nil {
		return policy
	}
	status := hostErrorStatus(err)
	body := safeError(err)
	for _, rule := range s.currentErrorRules() {
		if rule.Status != status {
			continue
		}
		matched := false
		for _, value := range rule.Match {
			if value != "" && strings.Contains(body, value) {
				matched = true
				break
			}
		}
		if !matched {
			for _, pattern := range rule.MatchRegexr {
				if expression, err := regexp.Compile(pattern); pattern != "" && err == nil && expression.MatchString(body) {
					matched = true
					break
				}
			}
		}
		if !matched {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(rule.Action)) {
		case "stop":
			policy.Cooldown, policy.Stop = false, true
		case "continue":
			policy.Cooldown, policy.Stop = false, false
		case "stop-and-cooldown":
			policy.Cooldown, policy.Stop = true, true
		case "continue-and-cooldown":
			policy.Cooldown, policy.Stop = true, false
		default:
			continue
		}
		policy.Explicit = true
		return policy
	}
	return policy
}

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
	if err.Kind == upstreamModelKind {
		body, _ := json.Marshal(map[string]any{"error": map[string]any{
			"message": "Requested model is unavailable for this credential",
			"type":    "not_found_error", "code": "model_not_found",
		}})
		return string(body)
	}
	if !containsHostTransportMarker(message) && !json.Valid([]byte(message)) {
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
	return requestFaultHostMessage(413, "message_too_big", message)
}

// JSON survives the stock host's string-only stream boundary and keeps a
// deterministic request rejection from rotating or penalizing credentials.
func requestFaultHostMessage(status int, code, message string) string {
	// Stock CPA checks some model/OAuth/challenge phrases before checking the
	// JSON request type. Do not let a rejected parameter mentioning one of those
	// phrases become a credential fault again. Full diagnostics stay in logs.
	lower := strings.ToLower(message)
	for _, marker := range []string{
		"invalid_grant", "model_not_found", "unknown_model", "unsupported model", "model not supported",
		"model is not supported", "requested model is unsupported", "requested model is unavailable", "model unavailable",
		"not available for your plan", "not available for your account", "challenge-platform", "cf-mitigated", "cloudflare challenge",
	} {
		if strings.Contains(lower, marker) {
			message = "Request parameters were rejected"
			break
		}
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": fmt.Sprintf("HTTP %d: %s", status, message),
		"type":    "invalid_request_error", "code": code,
	}})
	return string(body)
}
