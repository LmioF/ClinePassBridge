package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

const transportIncompleteFixture = `litellm.APIConnectionError: FireworksException - Response payload is not completed: <TransferEncodingError: 400, message='Not enough data to satisfy transfer length header.'>`

func TestProviderTransportIncompleteClassification(t *testing.T) {
	for _, tt := range []struct {
		name, message, kind string
		httpStatus, status  int
		bodyStatus          int
	}{
		{"HTTP 200 nested text 400", transportIncompleteFixture, transportIncompleteKind, 200, 502, 0},
		{"explicit SSE 502", transportIncompleteFixture, transportIncompleteKind, 200, 502, 502},
		{"HTTP 401 wins", transportIncompleteFixture, "upstream_auth_error", 401, 401, 502},
		{"HTTP 402 wins", transportIncompleteFixture, "upstream_error", 402, 402, 0},
		{"HTTP 403 wins", transportIncompleteFixture, "upstream_auth_error", 403, 403, 0},
		{"HTTP 429 wins", transportIncompleteFixture, "upstream_rate_limited", 429, 429, 502},
		{"SSE 401 wins", transportIncompleteFixture, "upstream_auth_error", 200, 401, 401},
		{"SSE 403 wins", transportIncompleteFixture, "upstream_auth_error", 200, 403, 403},
		{"SSE 429 wins", transportIncompleteFixture, "upstream_rate_limited", 200, 429, 429},
		{"account quota wins", transportIncompleteFixture + " reached your weekly Clinepass limit", "upstream_quota_exhausted", 200, 502, 0},
		{"ordinary 502", "Fireworks service unavailable", "upstream_error", 502, 502, 0},
		{"generic truncation", "TransferEncodingError: response payload incomplete", "upstream_error", 200, 502, 0},
		{"missing provider signature", "APIConnectionError: response payload incomplete", "upstream_error", 200, 502, 0},
		{"connection error alone", "litellm.APIConnectionError: connection failed", "upstream_error", 200, 502, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := map[string]any{"error": map[string]any{"message": tt.message}}
			if tt.bodyStatus != 0 {
				object(body["error"])["status"] = tt.bodyStatus
			}
			// Exercise the production decoder, which preserves statuses as json.Number.
			body, decodeErr := decodeObject(jsonBytes(body))
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			err := classifyUpstreamError(tt.httpStatus, http.Header{"Retry-After": []string{"120"}}, body, time.Now())
			upstream := err.(*upstreamError)
			if upstream.Kind != tt.kind || upstream.Status != tt.status || upstream.HTTPStatus != tt.httpStatus || upstream.UpstreamStatus != tt.status {
				t.Fatalf("classification = %#v", upstream)
			}
			if upstream.IsRequestScoped() != (tt.kind == transportIncompleteKind) {
				t.Fatalf("unexpected request scope for %s", tt.kind)
			}
			if upstream.IsRequestScoped() && (upstream.Scope != "request" || !upstream.RetryAt.IsZero() || asTeamRateLimit(err) != nil) {
				t.Fatalf("truncated transport acquired a cooldown: %#v", upstream)
			}
			if upstream.IsRequestScoped() && upstream.Message != tt.message {
				t.Fatal("classification rewrote the original exception text")
			}
		})
	}
}

func TestTransportIncompleteHonorsNumericErrorStatus(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		for _, value := range []any{status, int64(status), float64(status), json.Number(fmt.Sprint(status)), fmt.Sprint(status)} {
			err := classifyUpstreamError(200, nil, map[string]any{"error": map[string]any{"message": transportIncompleteFixture, "status": value}}, time.Now())
			upstream := err.(*upstreamError)
			if upstream.Status != status || upstream.UpstreamStatus != status || upstream.IsRequestScoped() {
				t.Errorf("status %v (%T) lost priority: %#v", value, value, upstream)
			}
		}
	}
}

func TestTransportIncompleteStreamBoundary(t *testing.T) {
	for _, tt := range []struct {
		name              string
		output, supported bool
	}{
		{"before output new host", false, true},
		{"after output new host", true, true},
		{"before output legacy host", false, false},
		{"after output legacy host", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			frames := [][]byte{sseFrame(map[string]any{"error": map[string]any{"message": transportIncompleteFixture, "status": 502}})}
			if tt.output {
				frames = append([][]byte{sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "thinking"}}}})}, frames...)
			}
			s := registeredService(t, "stream-aggregate")
			h := newFakeHost(ssePlan(frames...))
			var closePayload map[string]any
			s.SetHost(func(method string, payload, out any) error {
				if method == "host.stream.close" {
					closePayload = payload.(map[string]any)
					*out.(*streamCloseResponse) = streamCloseResponse{ErrorDetailsSupported: tt.supported}
				}
				return h.call(method, payload, out)
			})
			_, err := s.Handle("executor.execute_stream", executorRequest("client-1"))
			if tt.output && err != nil {
				t.Fatalf("stream handoff failed: %v", err)
			}
			if !tt.output {
				var upstream *upstreamError
				if !errors.As(err, &upstream) || !upstream.IsRequestScoped() || upstream.Code() != transportIncompleteKind {
					t.Fatalf("early RPC lost no-cooling code: %v", err)
				}
			}
			select {
			case <-h.clientClosed:
			case <-time.After(3 * time.Second):
				t.Fatal("output stream was not closed")
			}
			// Closing is acknowledged before the final log is persisted.
			s.active.Wait()
			details, ok := closePayload["error_details"].(streamErrorDetails)
			phase := "before_output"
			if tt.output {
				phase = "after_output"
			}
			if !ok || details.HTTPStatus != 502 || details.Code != transportIncompleteKind || details.Scope != "request" || !details.RequestScoped || details.Phase != phase || details.RetryAt != nil || str(closePayload["error"]) == "" {
				t.Fatalf("close protocol = %#v", closePayload)
			}
			if str(closePayload["error"]) != ClientErrorMessage(classifyUpstreamError(200, nil, map[string]any{"error": transportIncompleteFixture}, time.Now())) {
				t.Fatalf("client close message retained nested exception codes: %#v", closePayload)
			}
			if len(h.opened) != 1 || len(s.logs) != 1 {
				t.Fatalf("unexpected retry or missing final log: opens=%d logs=%d", len(h.opened), len(s.logs))
			}
			entry := s.logs[0]
			disposition := "exempt"
			if !tt.supported {
				disposition = "legacy_host_unknown"
			}
			if entry.ErrorPhase != phase || entry.DownstreamStarted == nil || *entry.DownstreamStarted != tt.output || entry.CooldownDisposition != disposition || entry.StreamErrorDetailsSupported == nil || *entry.StreamErrorDetailsSupported != tt.supported {
				t.Fatalf("boundary/compatibility log = %#v", entry)
			}
			if entry.PromptUsageReported == nil || *entry.PromptUsageReported || entry.CompletionUsageReported == nil || *entry.CompletionUsageReported || entry.UsageReported {
				t.Fatalf("missing usage reported as explicit zero: %#v", entry)
			}
			if tt.output && (entry.EffectiveFrameCount != 1 || entry.LastEffectiveFrameAt == nil) {
				t.Fatalf("meaningful reasoning frame not counted: %#v", entry)
			}
			if len(s.rateLimits.windows) != 0 || len(s.getEgressManager().groups) != 0 {
				t.Fatal("transport error created local team or egress backoff")
			}
		})
	}
}

func TestTransportIncompleteAggregateAndUsageFacts(t *testing.T) {
	for _, reported := range []bool{false, true} {
		t.Run(map[bool]string{false: "usage missing", true: "explicit zero usage"}[reported], func(t *testing.T) {
			frames := [][]byte{sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "partial"}}}})}
			if reported {
				frames = append(frames, sseFrame(map[string]any{"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0}}))
			}
			frames = append(frames, sseFrame(map[string]any{"error": map[string]any{"message": transportIncompleteFixture}}))
			s := registeredService(t, "stream-aggregate")
			h := newFakeHost(ssePlan(frames...))
			s.SetHost(h.call)
			result, err := s.Handle("executor.execute", executorRequest(""))
			if err == nil || result != nil || len(h.opened) != 1 || len(h.emitted) != 0 || len(s.logs) != 1 {
				t.Fatalf("aggregate leaked output/retried: result=%#v err=%v opens=%d", result, err, len(h.opened))
			}
			entry := s.logs[0]
			if entry.DownstreamStarted == nil || *entry.DownstreamStarted || entry.ErrorPhase != "before_output" || entry.CooldownDisposition != "legacy_host_unknown" || entry.EffectiveFrameCount != 1 {
				t.Fatalf("aggregate boundary = %#v", entry)
			}
			if entry.PromptUsageReported == nil || *entry.PromptUsageReported != reported || entry.CompletionUsageReported == nil || *entry.CompletionUsageReported != reported || entry.UsageReported != reported || entry.PromptTokens != 0 || entry.CompletionTokens != 0 {
				t.Fatalf("usage presence = %#v", entry)
			}
		})
	}
	var legacy LogEntry
	if err := json.Unmarshal([]byte(`{"id":"legacy","prompt_tokens":0,"completion_tokens":0}`), &legacy); err != nil || legacy.DownstreamStarted != nil || legacy.PromptUsageReported != nil || legacy.CompletionUsageReported != nil {
		t.Fatalf("legacy log acquired invented observations: %#v, %v", legacy, err)
	}
}

func TestTransportIncompleteDoesNotRetreatRecoveryProbe(t *testing.T) {
	now := time.Now()
	l := rateLimiter{clock: func() time.Time { return now }}
	c := Credential{ID: "fixture", APIKey: "fixture-key"}
	lease, err := l.acquire(c, "model")
	if err != nil {
		t.Fatal(err)
	}
	lease.finish(classifyUpstreamError(429, http.Header{"Retry-After": []string{"1"}}, map[string]any{"error": teamLimitMessage}, now))
	now = now.Add(2 * time.Second)
	probe, err := l.acquire(c, "model")
	if err != nil {
		t.Fatal(err)
	}
	deadline := l.windows[probe.key].retryAt
	finishRequestRateLease(probe, classifyUpstreamError(200, nil, map[string]any{"error": transportIncompleteFixture}, now))
	state := l.windows[probe.key]
	if state.probe != nil || !state.retryAt.Equal(deadline) {
		t.Fatalf("transport error extended team retreat: %#v", state)
	}
}

func TestStreamClosePreservesOrdinaryErrorPolicy(t *testing.T) {
	now := time.Now().UTC()
	for _, tt := range []struct {
		name   string
		status int
		scope  string
		err    error
	}{
		{"authentication", 401, "", classifyUpstreamError(401, nil, map[string]any{"error": "invalid authentication"}, now)},
		{"rate limit", 429, "credential_model", classifyUpstreamError(429, nil, map[string]any{"error": "rate limited"}, now)},
		{"team limit", 429, "upstream_team", classifyUpstreamError(429, nil, map[string]any{"error": teamLimitMessage}, now)},
		{"account quota", 502, "account", classifyUpstreamError(502, nil, map[string]any{"error": "reached your weekly Clinepass limit"}, now)},
		{"ordinary gateway", 502, "", classifyUpstreamError(502, nil, map[string]any{"error": "bad gateway"}, now)},
		{"egress limit", 429, "egress_ip", &upstreamError{APIError: &APIError{429, "upstream_ip_rate_limited", "edge rate limit"}, Scope: "egress_ip", RetryAt: now.Add(time.Minute)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := NewService()
			var details streamErrorDetails
			s.SetHost(func(method string, payload, out any) error {
				if method != "host.stream.close" {
					t.Fatalf("unexpected callback %s", method)
				}
				details = payload.(map[string]any)["error_details"].(streamErrorDetails)
				*out.(*streamCloseResponse) = streamCloseResponse{ErrorDetailsSupported: true}
				return nil
			})
			entry := LogEntry{DownstreamStarted: new(bool)}
			logErrorDetails(&entry, nil, tt.err)
			s.closeOutputStream("fixture", &entry, tt.err)
			if details.HTTPStatus != tt.status || details.Scope != tt.scope || details.RequestScoped || entry.CooldownDisposition != "host_policy" {
				t.Fatalf("ordinary error lost its policy: details=%#v log=%#v", details, entry)
			}
			upstream := tt.err.(*upstreamError)
			if details.Code != upstream.Kind || (details.RetryAt == nil) != upstream.RetryAt.IsZero() || (details.RetryAt != nil && !details.RetryAt.Equal(upstream.RetryAt)) {
				t.Fatalf("close lost code or retry time: %#v", details)
			}
		})
	}
}

func TestDownstreamStartedRequiresSuccessfulEmit(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(map[int]string{1: "first emit fails", 2: "later emit fails"}[failAt], func(t *testing.T) {
			s := registeredService(t, "stream-aggregate")
			content := sseFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "partial"}}}})
			h := newFakeHost(ssePlan(content, content))
			h.emitFailAt = failAt
			s.SetHost(h.call)
			if _, err := s.Handle("executor.execute_stream", executorRequest("client-1")); err != nil {
				t.Fatalf("existing stream handoff changed: %v", err)
			}
			select {
			case <-h.clientClosed:
			case <-time.After(3 * time.Second):
				t.Fatal("canceled stream was not closed")
			}
			s.active.Wait()
			entry := s.logs[0]
			phase := "before_output"
			if failAt == 2 {
				phase = "after_output"
			}
			if entry.DownstreamStarted == nil || *entry.DownstreamStarted != (failAt == 2) || entry.ErrorPhase != phase || entry.EffectiveFrameCount != failAt || entry.LastEffectiveFrameAt == nil || entry.Status != 499 || entry.ErrorKind != "request_canceled" || len(h.opened) != 1 {
				t.Fatalf("upstream observations mistaken for successful emit: %#v", entry)
			}
		})
	}
}

func TestTransportIncompleteNeverTriggersNativeFallback(t *testing.T) {
	s := registeredService(t, "native-fallback")
	h := newFakeHost(jsonPlan(map[string]any{"error": map[string]any{"message": transportIncompleteFixture + " empty response content"}}))
	s.SetHost(h.call)
	result, err := s.Handle("executor.execute", executorRequest(""))
	var upstream *upstreamError
	if result != nil || !errors.As(err, &upstream) || !upstream.IsRequestScoped() || len(h.opened) != 1 || len(s.logs) != 1 || len(s.logs[0].Attempts) != 1 {
		t.Fatalf("transport error triggered fallback: result=%#v err=%v opens=%d", result, err, len(h.opened))
	}
}
