package bridge

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestStreamServiceErrorRetryCompatibilityBoundaries(t *testing.T) {
	const original = "Stream error occurred"
	serviceErr := classifyUpstreamError(200, nil, map[string]any{"error": original}, time.Now())
	for _, tc := range []struct {
		name, path, phase, action string
		disabled, compatible      bool
		err                       error
	}{
		{name: "chat service error", path: "/v1/chat/completions", phase: "after_output", compatible: true, err: serviceErr},
		{name: "wrapped service error", path: "/v1/chat/completions", phase: "after_output", compatible: true, err: fmt.Errorf("wrapped: %w", serviceErr)},
		{name: "compatibility disabled", path: "/v1/chat/completions", phase: "after_output", disabled: true, err: serviceErr},
		{name: "missing request path", phase: "after_output", err: serviceErr},
		{name: "Claude protocol", path: "/v1/messages", phase: "after_output", err: serviceErr},
		{name: "Responses protocol", path: "/v1/responses", phase: "after_output", err: serviceErr},
		{name: "before output", path: "/v1/chat/completions", phase: "before_output", err: serviceErr},
		{name: "protocol failure", path: "/v1/chat/completions", phase: "after_output", err: upstreamProtocolFailure(original)},
		{name: "unclassified local failure", path: "/v1/chat/completions", phase: "after_output", err: fail(502, original)},
		{name: "non-service status", path: "/v1/chat/completions", phase: "after_output", err: requestFault(418, upstreamServiceKind, original)},
		{name: "authentication", path: "/v1/chat/completions", phase: "after_output", err: classifyUpstreamError(401, nil, map[string]any{"error": original}, time.Now())},
		{name: "account quota", path: "/v1/chat/completions", phase: "after_output", err: classifyUpstreamError(502, nil, map[string]any{"error": "reached your weekly Clinepass limit"}, time.Now())},
		{name: "model failure", path: "/v1/chat/completions", phase: "after_output", err: classifyUpstreamError(502, nil, map[string]any{"error": map[string]any{"code": "model_not_found", "message": original}}, time.Now())},
		{name: "account scoped service error", path: "/v1/chat/completions", phase: "after_output", err: &upstreamError{APIError: &APIError{Status: 502, Kind: upstreamServiceKind, Message: original}, Scope: "account"}},
		{name: "explicit stop", path: "/v1/chat/completions", phase: "after_output", action: "stop", err: serviceErr},
		{name: "explicit continue", path: "/v1/chat/completions", phase: "after_output", action: "continue", err: serviceErr},
		{name: "explicit cooldown", path: "/v1/chat/completions", phase: "after_output", action: "continue-and-cooldown", err: serviceErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService()
			s.cfg.ClientRetryCompatibility = !tc.disabled
			s.errorRules = nil
			if tc.action != "" {
				s.errorRules = []RequestErrorRule{{Status: 502, Match: []string{original}, Action: tc.action}}
			}
			h := newFakeHost()
			closes := 0
			var details streamErrorDetails
			s.SetHost(func(method string, payload, out any) error {
				if method != "host.stream.close" {
					t.Fatalf("terminal error unexpectedly used callback %s", method)
				}
				closes++
				details = payload.(map[string]any)["error_details"].(streamErrorDetails)
				return h.call(method, payload, out)
			})
			legacyMessage, _ := s.hostErrorDetails(tc.err, true)
			rpcMessage, rpcStatus := s.HostErrorDetails(tc.err)
			entry := LogEntry{ErrorPhase: tc.phase}
			s.closeOutputStream("fixture", tc.path, &entry, tc.err)
			if closes != 1 || details.HTTPStatus != statusOf(tc.err) || details.Phase != tc.phase {
				t.Fatalf("stream close changed diagnostic protocol: closes=%d details=%#v", closes, details)
			}
			if !tc.compatible {
				if h.clientError != legacyMessage {
					t.Fatalf("unrelated close changed: got %q, want %q", h.clientError, legacyMessage)
				}
				return
			}
			var body struct {
				Error struct{ Type, Code, Message string }
			}
			if err := json.Unmarshal([]byte(h.clientError), &body); err != nil {
				t.Fatalf("host close did not receive a complete JSON error: %q (%v)", h.clientError, err)
			}
			if body.Error.Type != "server_error" || body.Error.Code != "500" || body.Error.Message != legacyMessage || !strings.Contains(body.Error.Message, "[clinepassbridge:cpa_compat:server misbehaving]") || strings.ContainsAny(h.clientError, "\r\n") {
				t.Fatalf("client retry classification or single-line framing lost: %q", h.clientError)
			}
			if details.Code != upstreamServiceKind || details.Scope != "request" || !details.RequestScoped || details.RetryAt != nil {
				t.Fatalf("client compatibility changed host error details: %#v", details)
			}
			if message, status := s.HostErrorDetails(tc.err); message != rpcMessage || status != rpcStatus || status != 0 || json.Valid([]byte(message)) {
				t.Fatalf("stream compatibility changed the RPC boundary: status=%d message=%q", status, message)
			}
		})
	}
}

func TestExecuteStreamServiceErrorPreservesDiagnosticsAndOnlyClosesOnce(t *testing.T) {
	const original = "Stream error occurred"
	s := registeredService(t, "stream-aggregate")
	h := newFakeHost(ssePlan(
		idleDeltaFrame(map[string]any{"content": "partial response"}),
		sseFrame(map[string]any{"error": original}),
	))
	closes := 0
	var details streamErrorDetails
	s.SetHost(func(method string, payload, out any) error {
		if method == "host.stream.close" {
			closes++
			details = payload.(map[string]any)["error_details"].(streamErrorDetails)
		}
		return h.call(method, payload, out)
	})
	var request ExecutorRequest
	if err := json.Unmarshal(executorRequest("client-1"), &request); err != nil {
		t.Fatal(err)
	}
	request.Metadata = map[string]any{"request_path": "/v1/chat/completions"}
	if _, err := s.Handle("executor.execute_stream", jsonBytes(request)); err != nil {
		t.Fatalf("stream handoff failed: %v", err)
	}
	select {
	case <-h.clientClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("failed upstream did not close the output stream")
	}
	s.active.Wait()
	if closes != 1 || len(h.opened) != 1 || len(h.emitted) != 1 {
		t.Fatalf("terminal failure was replayed or emitted as extra data: closes=%d requests=%d chunks=%d", closes, len(h.opened), len(h.emitted))
	}
	var frame struct {
		Choices []struct{ Delta struct{ Content string } }
	}
	if err := json.Unmarshal(h.emitted[0], &frame); err != nil || len(frame.Choices) != 1 || frame.Choices[0].Delta.Content != "partial response" {
		t.Fatalf("delivered content changed: %s (%v)", h.emitted[0], err)
	}
	var body struct {
		Error struct{ Type, Code, Message string }
	}
	if err := json.Unmarshal([]byte(h.clientError), &body); err != nil || body.Error.Type != "server_error" || body.Error.Code != "500" || !strings.Contains(body.Error.Message, "upstream_service_error") {
		t.Fatalf("request path did not reach the terminal compatibility encoder: %q (%v)", h.clientError, err)
	}
	if details.HTTPStatus != 502 || details.Code != upstreamServiceKind || details.Scope != "request" || !details.RequestScoped || details.Phase != "after_output" {
		t.Fatalf("terminal host diagnostics changed: %#v", details)
	}
	if len(s.logs) != 1 {
		t.Fatalf("terminal logs=%d", len(s.logs))
	}
	entry := s.logs[0]
	if entry.Error != original || entry.Status != 502 || entry.ErrorKind != upstreamServiceKind || entry.UpstreamHTTPStatus != 200 || entry.UpstreamErrorStatus != 502 || entry.ErrorPhase != "after_output" || entry.DownstreamStarted == nil || !*entry.DownstreamStarted || entry.EffectiveFrameCount != 1 {
		t.Fatalf("original upstream error or output phase lost: %#v", entry)
	}
	if entry.UpstreamAttempts == nil || *entry.UpstreamAttempts != 1 || len(entry.Attempts) != 1 || entry.Attempts[0].Error != original || entry.Attempts[0].ErrorKind != upstreamServiceKind || entry.CooldownDisposition != "legacy_host_unknown" {
		t.Fatalf("client compatibility changed attempts or host receipt: %#v", entry)
	}
}
