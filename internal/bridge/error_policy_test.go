package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

type errorPolicyStatusError struct {
	message string
	status  int
}

func (e errorPolicyStatusError) Error() string   { return e.message }
func (e errorPolicyStatusError) StatusCode() int { return e.status }

type errorPolicyRequestCase struct {
	name, marker string
	err          error
	rpcStatus    int
}

func errorPolicyRequestCases() []errorPolicyRequestCase {
	return []errorPolicyRequestCase{
		{"service", "server misbehaving", classifyUpstreamError(200, nil, map[string]any{"error": "Stream error occurred"}, time.Now()), 0},
		{"protocol", "server misbehaving", upstreamProtocolFailure("invalid SSE response"), 0},
		{"callback", "connection aborted", hostHTTPFault(errors.New("HTTP/2 INTERNAL_ERROR"), nil, "upstream stream interrupted"), 0},
		{"network", "connection aborted", upstreamNetworkFailure("eof", "upstream stream closed before [DONE]"), 0},
		{"shutdown", "server misbehaving", bridgeUnavailable("ClinePassBridge is shutting down"), 0},
		{"cancel", "context canceled", requestCanceled("client disconnected"), 0},
		{"deadline", "context deadline exceeded", requestTimedOut("upstream deadline elapsed"), 0},
		{"response_limit", "message_too_big", responseLimitFailure("upstream response exceeds configured limit"), 413},
	}
}

func TestErrorPolicyRequestWireBoundaries(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, tc := range errorPolicyRequestCases() {
			t.Run(fmt.Sprintf("%s/compat=%v", tc.name, enabled), func(t *testing.T) {
				s := NewService()
				s.cfg.ClientRetryCompatibility = enabled
				h := newFakeHost()
				s.SetHost(h.call)
				original, diagnosticStatus := tc.err.Error(), statusOf(tc.err)
				wrapped := fmt.Errorf("wrapped: %w", tc.err)
				var upstream *upstreamError
				if !errors.As(wrapped, &upstream) || !upstream.IsRequestScoped() || upstream.Scope != "request" {
					t.Fatalf("request fault lost scope: %#v", tc.err)
				}
				message, status := s.HostErrorDetails(wrapped)
				if status != tc.rpcStatus || !strings.Contains(message, tc.marker) {
					t.Fatalf("wire status=%d message=%q", status, message)
				}
				if tc.name == "cancel" || tc.name == "deadline" {
					if message != tc.marker {
						t.Fatalf("termination message must be exact: %q", message)
					}
				}
				if tc.name == "response_limit" {
					var body struct {
						Error struct{ Message, Type, Code string }
					}
					if err := json.Unmarshal([]byte(message), &body); err != nil || body.Error.Type != "invalid_request_error" || body.Error.Code != "message_too_big" || !strings.HasPrefix(body.Error.Message, "HTTP 413:") {
						t.Fatalf("response limit is not a deterministic client fault: %q (%v)", message, err)
					}
				} else if tc.name != "cancel" && tc.name != "deadline" && !strings.Contains(message, "[clinepassbridge:cpa_compat:") {
					t.Fatalf("missing explicit compatibility tag: %q", message)
				}
				s.closeOutputStream("fixture", &LogEntry{ErrorPhase: "after_output"}, wrapped)
				if h.clientError != message || tc.err.Error() != original || statusOf(tc.err) != diagnosticStatus {
					t.Fatal("RPC/stream encoding diverged or modified diagnostic facts")
				}
			})
		}
	}
}

func TestErrorPolicyProtectedStatusAndScopePrecedence(t *testing.T) {
	now := time.Now()
	for _, outer := range []int{200, 502} {
		for _, status := range []int{401, 402, 403, 429} {
			body := map[string]any{"error": map[string]any{"message": transportIncompleteFixture, "status": fmt.Sprint(status)}}
			upstream := classifyUpstreamError(outer, nil, body, now).(*upstreamError)
			if upstream.Status != status || upstream.HTTPStatus != outer || upstream.IsRequestScoped() {
				t.Fatalf("outer=%d protected=%d: %#v", outer, status, upstream)
			}
		}
	}
	for _, tc := range []struct {
		name   string
		body   map[string]any
		status int
	}{
		{"authentication code", map[string]any{"error": map[string]any{"code": "authentication_error", "message": "unexpected EOF"}}, 401},
		{"permission type", map[string]any{"error": map[string]any{"type": "permission_denied", "message": "i/o timeout"}}, 403},
		{"rate type", map[string]any{"type": "rate_limit_error", "error": "connection reset"}, 429},
		{"substring is not an auth code", map[string]any{"error": map[string]any{"code": "other_authentication_error", "message": "service unavailable"}}, 502},
		{"nested transport 400 remains transport", map[string]any{"error": map[string]any{"status": 400, "message": transportIncompleteFixture}}, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := classifyUpstreamError(502, nil, tc.body, now).(*upstreamError)
			if upstream.Status != tc.status || upstream.IsRequestScoped() != (tc.status == 502) {
				t.Fatalf("protected structure = %#v", upstream)
			}
		})
	}
	for _, kind := range []string{transportIncompleteKind, upstreamStreamIdleTimeoutKind, upstreamServiceKind, upstreamProtocolKind, upstreamCallbackKind, upstreamNetworkKind, responseLimitKind, bridgeUnavailableKind, requestCanceledKind, requestDeadlineKind} {
		for _, scope := range []string{"account", "credential_model", "upstream_team"} {
			upstream := &upstreamError{APIError: &APIError{Status: 502, Kind: kind, Message: "scope fixture"}, Scope: scope}
			if upstream.IsRequestScoped() {
				t.Fatalf("kind=%s scope=%s acquired request exemption", kind, scope)
			}
		}
		for _, status := range []int{401, 402, 403, 429} {
			upstream := &upstreamError{APIError: &APIError{Status: status, Kind: kind, Message: "status fixture"}, Scope: "request"}
			if upstream.IsRequestScoped() {
				t.Fatalf("kind=%s status=%d acquired request exemption", kind, status)
			}
		}
	}
}

func TestErrorPolicyProtectedWireIsolatesTransportText(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		s := NewService()
		s.cfg.ClientRetryCompatibility = enabled
		s.errorRules = nil // Exercise baseline protections without an explicit rule override.
		for _, marker := range []string{"unexpected EOF", "i/o timeout", "connection reset", "server misbehaving", "http2: client connection force closed via ClientConn.Close", "http2: client conn is closed"} {
			for _, tc := range []struct {
				name    string
				status  int
				message string
			}{
				{"auth", 401, marker}, {"payment", 402, marker}, {"permission", 403, marker}, {"rate", 429, marker},
				{"quota", 502, marker + " reached your weekly Clinepass limit"},
				{"team", 429, marker + " " + teamLimitMessage},
			} {
				err := classifyUpstreamError(tc.status, http.Header{"Retry-After": {"120"}}, map[string]any{"error": tc.message}, time.Now())
				message, status := s.HostErrorDetails(err)
				if err.(*upstreamError).IsRequestScoped() || status != statusOf(err) || strings.Contains(strings.ToLower(message), strings.ToLower(marker)) || strings.Contains(message, "cpa_compat") {
					t.Fatalf("%s/compat=%v: protected text leaked: status=%d message=%q", tc.name, enabled, status, message)
				}
				if tc.name == "team" && !strings.Contains(message, rateLimitMarker) {
					t.Fatalf("team rule marker lost: %q", message)
				}
				if (tc.name == "rate" || tc.name == "team") && !strings.Contains(message, "Retry after 120s") {
					t.Fatalf("protected rate retry hint lost: %q", message)
				}
			}
		}
	}
}

func TestErrorPolicySourceClassifiesProtocolAndLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan hostPlan
		max  int
		kind string
	}{
		{"invalid SSE JSON", ssePlan([]byte("data: {invalid}\n\n")), 0, upstreamProtocolKind},
		{"missing finish reason", ssePlan([]byte("data: [DONE]\n\n")), 0, upstreamProtocolKind},
		{"non SSE content", jsonPlan(map[string]any{"choices": []any{}}), 0, upstreamProtocolKind},
		{"response cap", ssePlan(sseFrame(map[string]any{"choices": []any{}, "padding": strings.Repeat("x", 256)})), 128, responseLimitKind},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService()
			if tc.max > 0 {
				s.cfg.MaxResponseBytes = tc.max
			}
			h := newFakeHost(tc.plan)
			s.SetHost(h.call)
			up, err := s.openUpstream(map[string]any{"body": []byte(`{}`)}, time.Now().Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.consumeSSE(up, "fixture", &LogEntry{}, &Attempt{}, time.Now(), nil)
			var upstream *upstreamError
			if !errors.As(err, &upstream) || upstream.Kind != tc.kind || !upstream.IsRequestScoped() {
				t.Fatalf("source policy = %#v", err)
			}
		})
	}
}
