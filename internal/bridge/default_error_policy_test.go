package bridge

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDefaultErrorPolicyUnknownAndProtectedWire(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		cooldown bool
		status   int
	}{
		{"raw failure", errors.New("unclassified local processing failure"), false, 0},
		{"local 503", fail(503, "unclassified local failure"), false, 0},
		{"unknown status", fail(418, "unrecognized upstream failure"), false, 0},
		{"auth", fail(401, "invalid key"), true, 401},
		{"permission", fail(403, "permission denied"), true, 403},
		{"rate", fail(429, "rate limit"), true, 429},
		{"raw model code", &APIError{400, "model_not_found", "unavailable"}, true, 400},
		{"model code with plain JSON", &APIError{400, "model_not_found", `{"error":{"message":"unavailable"}}`}, true, 400},
		{"grant code with plain JSON", &APIError{400, "invalid_grant", `{"error":{"message":"OAuth token rejected"}}`}, true, 401},
		{"raw grant body", errors.New(`{"error":{"code":"invalid_grant","message":"expired"}}`), true, 401},
		{"typed response owns its quoted message", requestFault(502, upstreamProtocolKind, `{"error":{"code":"invalid_grant"}}`), false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService()
			policy := faultPolicyFor(tc.err)
			if policy.Cooldown != tc.cooldown {
				t.Fatalf("policy = %+v", policy)
			}
			message, status := s.HostErrorDetails(tc.err)
			if status != tc.status {
				t.Fatalf("wire status=%d message=%q", status, message)
			}
			if !tc.cooldown && !strings.Contains(message, "[clinepassbridge:cpa_compat:") {
				t.Fatalf("unknown failure lacks host exemption: %q", message)
			}
			if tc.cooldown && containsHostTransportMarker(message) {
				t.Fatalf("protected failure acquired an exemption: %q", message)
			}
		})
	}
}

func TestDefaultErrorPolicySSERequestSurvivesStringBoundary(t *testing.T) {
	for _, status := range []int{400, 422} {
		s := NewService()
		h := newFakeHost(ssePlan(
			sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hello"}}}}),
			sseFrame(map[string]any{"error": map[string]any{"status": status, "type": "invalid_request_error", "message": "invalid tool arguments"}}),
		))
		s.SetHost(h.call)
		up, err := s.openUpstream(map[string]any{"body": []byte(`{}`)}, time.Now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		entry := &LogEntry{}
		_, err = s.consumeSSE(up, "fixture", entry, &Attempt{}, time.Now(), func([]byte) error { return nil })
		if err == nil || entry.EffectiveFrameCount == 0 {
			t.Fatal("expected a request rejection after model output")
		}
		s.sendOutputStreamClose("fixture", "after_output", err)
		var body struct {
			Error struct{ Type, Message string }
		}
		if decodeErr := json.Unmarshal([]byte(h.clientError), &body); decodeErr != nil || body.Error.Type != "invalid_request_error" || !strings.Contains(body.Error.Message, "invalid tool arguments") {
			t.Fatalf("request type lost at string boundary: %q (%v)", h.clientError, decodeErr)
		}
	}
}

func TestDefaultErrorPolicyExplicitRulesOverrideBothWires(t *testing.T) {
	for _, tc := range []struct {
		action   string
		cooldown bool
		stop     bool
	}{
		{"stop", false, true},
		{"continue", false, false},
		{"stop-and-cooldown", true, true},
		{"continue-and-cooldown", true, false},
	} {
		t.Run(tc.action, func(t *testing.T) {
			s := NewService()
			s.errorRules = []RequestErrorRule{
				{Status: 502, MatchRegexr: []string{"^fixture failure$"}, Action: tc.action},
				{Status: 502, Match: []string{"fixture"}, Action: "stop"},
			}
			err := upstreamProtocolFailure("fixture failure")
			policy := s.hostFaultPolicy(err)
			if !policy.Explicit || policy.Cooldown != tc.cooldown || policy.Stop != tc.stop {
				t.Fatalf("first matching rule lost: %+v", policy)
			}
			message, status := s.HostErrorDetails(err)
			if status != 502 || message != err.Error() {
				t.Fatalf("RPC rule no longer matchable: status=%d message=%q", status, message)
			}
			h := newFakeHost()
			s.SetHost(h.call)
			s.sendOutputStreamClose("fixture", "after_output", err)
			var body struct{ Error struct{ Type string } }
			jsonErr := json.Unmarshal([]byte(h.clientError), &body)
			if tc.cooldown {
				if containsHostTransportMarker(h.clientError) || (jsonErr == nil && body.Error.Type == "invalid_request_error") {
					t.Fatalf("force-cooldown stream acquired exemption: %q", h.clientError)
				}
			} else if tc.stop {
				if jsonErr != nil || body.Error.Type != "invalid_request_error" {
					t.Fatalf("stop stream lost request classification: %q", h.clientError)
				}
			} else if !containsHostTransportMarker(h.clientError) || jsonErr == nil {
				t.Fatalf("continue stream lost transport fallback: %q", h.clientError)
			}
		})
	}
}
