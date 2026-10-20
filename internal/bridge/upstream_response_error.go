package bridge

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	upstreamRequestKind = "upstream_request_error"
	upstreamModelKind   = "upstream_model_unavailable"
	upstreamRouteKind   = "upstream_route_error"
	upstreamEdgeKind    = "upstream_edge_challenge"
)

// Keep message matching limited to an unavailable model as the subject. For
// example, "temperature is not supported by this model" is a request error.
var unavailableModelPattern = regexp.MustCompile("(?i)\\bmodel(?:\\s+(?:[\"'`][^\"'`\\r\\n]{1,128}[\"'`]|[a-z0-9][a-z0-9._:/-]{0,127}))?\\s+(?:is\\s+)?(?:unsupported|unavailable|not\\s+(?:found|supported|available)|does\\s+not\\s+exist|has\\s+been\\s+(?:removed|retired))\\b")
var unsupportedModelPrefixPattern = regexp.MustCompile(`(?i)^\s*unsupported\s+model\s*:\s*\S`)
var modelParameterMessagePattern = regexp.MustCompile(`(?i)\b(?:parameter|argument|field|setting|option)s?\b`)

func upstreamErrorCodes(body map[string]any) []string {
	detail := object(body["error"])
	result := make([]string, 0, 4)
	for _, value := range []any{detail["code"], detail["type"], body["code"], body["type"]} {
		result = append(result, strings.ToLower(strings.TrimSpace(str(value))))
	}
	return result
}

func isUpstreamCredentialError(body map[string]any) bool {
	for _, code := range upstreamErrorCodes(body) {
		switch code {
		case "authentication_error", "invalid_api_key", "invalid_grant", "invalid_token", "expired_token", "token_expired",
			"invalid_client", "unauthorized", "unauthorized_client", "access_denied", "permission_error", "permission_denied", "insufficient_permissions",
			"account_deactivated", "account_disabled", "account_suspended":
			return true
		}
	}
	return false
}

func isUpstreamModelError(body map[string]any) bool {
	for _, code := range upstreamErrorCodes(body) {
		switch code {
		case "model_not_found", "model_not_found_error", "unknown_model", "model_does_not_exist", "model_not_exist",
			"model_not_available", "model_unavailable", "unsupported_model", "unsupported_model_error",
			"model_not_supported", "model_unsupported", "invalid_model", "model_access_denied":
			return true
		}
	}
	message := strings.ToLower(errorMessage(body))
	if unsupportedModelPrefixPattern.MatchString(message) {
		return true
	}
	if modelParameterMessagePattern.MatchString(message) {
		return false
	}
	return unavailableModelPattern.MatchString(message)
}

func isUpstreamRequestError(status int, body map[string]any) bool {
	switch status {
	case 400, 409, 413, 415, 422:
		return true
	}
	for _, code := range upstreamErrorCodes(body) {
		switch code {
		case "invalid_request", "invalid_request_error", "bad_request", "bad_request_error", "validation_error", "request_validation_error",
			"invalid_argument", "invalid_parameter", "invalid_parameter_error", "missing_required_parameter", "unsupported_parameter", "unsupported_parameter_error",
			"unknown_parameter", "unrecognized_request_argument", "context_length_exceeded", "context_window_exceeded", "max_context_length_exceeded",
			"token_limit_exceeded", "input_too_long", "prompt_too_long", "request_too_large", "payload_too_large", "content_too_large",
			"request_entity_too_large", "unsupported_media_type", "unprocessable_entity":
			return true
		}
	}
	return false
}

// Classify response-wide routing/edge faults before applying any local egress
// cooldown. Host HTTP callbacks use the same path even without a local pool.
func classifyUpstreamResponseError(httpStatus int, headers http.Header, body map[string]any, raw []byte, now time.Time, fallback ...int) error {
	if body == nil && json.Valid(raw) {
		body, _ = decodeObject(raw)
	}
	result := classifyUpstreamError(httpStatus, headers, body, now, fallback...).(*upstreamError)
	if httpStatus == http.StatusNotFound && !json.Valid(raw) && isUpstreamTextResponse(headers, raw) {
		result.Status, result.Kind, result.Scope = http.StatusBadGateway, upstreamRouteKind, "request"
		result.UpstreamStatus = httpStatus
		return result
	}
	if httpStatus != http.StatusForbidden || !isUpstreamEdgeChallenge(headers, raw) {
		return result
	}
	// JSON error envelopes retain authentication, quota and team semantics even
	// when an intermediary adds HTML headers or challenge-looking message text.
	// Ambiguous JSON 403 errors also keep the conservative protected policy.
	if body != nil && (body["error"] != nil || body["success"] == false || isUpstreamCredentialError(body) || isUpstreamModelError(body) || quotaErrorType(strings.Join(upstreamErrorCodes(body), " "), errorMessage(body)) != "") {
		return result
	}
	result.Status, result.Kind, result.Scope = http.StatusBadGateway, upstreamEdgeKind, "egress_ip"
	result.UpstreamStatus = httpStatus
	return result
}

func isUpstreamTextResponse(headers http.Header, raw []byte) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	contentType := strings.ToLower(headers.Get("Content-Type"))
	if strings.Contains(contentType, "text/html") || strings.Contains(contentType, "text/plain") {
		return true
	}
	sniffed := http.DetectContentType(raw)
	return strings.HasPrefix(sniffed, "text/html") || strings.HasPrefix(sniffed, "text/plain")
}

func isUpstreamEdgeChallenge(headers http.Header, raw []byte) bool {
	for name, values := range headers {
		if strings.EqualFold(name, "Cf-Mitigated") {
			for _, value := range values {
				if strings.EqualFold(strings.TrimSpace(value), "challenge") {
					return true
				}
			}
		}
	}
	text := strings.ToLower(strings.TrimSpace(string(raw)))
	html := strings.Contains(strings.ToLower(headers.Get("Content-Type")), "text/html") || strings.HasPrefix(text, "<!doctype html") || strings.HasPrefix(text, "<html")
	if !html {
		return false
	}
	return strings.Contains(text, "/cdn-cgi/challenge-platform/") || strings.Contains(text, "_cf_chl_opt") || strings.Contains(text, "cf-chl-")
}
