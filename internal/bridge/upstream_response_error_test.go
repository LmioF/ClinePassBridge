package bridge

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestUpstreamResponseErrorRequestAndModelClassification(t *testing.T) {
	for _, tc := range []struct {
		name, code, message, kind, scope string
		httpStatus, status               int
	}{
		{"parameter", "invalid_request_error", "unsupported temperature", upstreamRequestKind, "request", 400, 400},
		{"conflict", "", "request conflicts with current state", upstreamRequestKind, "request", 409, 409},
		{"context", "context_length_exceeded", "context too long", upstreamRequestKind, "request", 413, 413},
		{"media", "", "unsupported content type", upstreamRequestKind, "request", 415, 415},
		{"validation", "", "messages is required", upstreamRequestKind, "request", 422, 422},
		{"successful transport request error", "invalid_request_error", "invalid parameter", upstreamRequestKind, "request", 200, 502},
		{"service envelope request error", "context_length_exceeded", "context too long", upstreamRequestKind, "request", 503, 503},
		{"model code", "model_not_found", "unknown model", upstreamModelKind, "credential_model", 400, 400},
		{"model not found error code", "model_not_found_error", "unknown selection", upstreamModelKind, "credential_model", 400, 400},
		{"unknown model code", "unknown_model", "unknown selection", upstreamModelKind, "credential_model", 422, 422},
		{"model does not exist code", "model_does_not_exist", "unknown selection", upstreamModelKind, "credential_model", 400, 400},
		{"model not exist code", "model_not_exist", "unknown selection", upstreamModelKind, "credential_model", 422, 422},
		{"unsupported model code", "unsupported_model", "unsupported selection", upstreamModelKind, "credential_model", 422, 422},
		{"model message", "invalid_request_error", "The model `fixture-model` does not exist or you do not have access to it", upstreamModelKind, "credential_model", 404, 404},
		{"unsupported named model", "", "Model fixture-model is not supported", upstreamModelKind, "credential_model", 400, 400},
		{"unsupported model prefix 400", "invalid_request_error", "Unsupported model: fixture-model", upstreamModelKind, "credential_model", 400, 400},
		{"unsupported model prefix 422", "", "Unsupported model: fixture-model", upstreamModelKind, "credential_model", 422, 422},
		{"unsupported model prefix named parameter", "", "Unsupported model: parameter-v1", upstreamModelKind, "credential_model", 400, 400},
		{"unsupported model parameter is a request", "invalid_request_error", "Unsupported model parameter: temperature", upstreamRequestKind, "request", 400, 400},
		{"quoted unsupported model prefix is a request", "invalid_request_error", "Invalid parameter. Unsupported model: fixture-model", upstreamRequestKind, "request", 422, 422},
		{"parameter is not a model error", "invalid_request_error", "Parameter temperature is not supported by this model", upstreamRequestKind, "request", 400, 400},
		{"invalid grant", "invalid_grant", "grant expired", "upstream_auth_error", "account", 400, 400},
		{"invalid token", "invalid_token", "token expired", "upstream_auth_error", "account", 422, 422},
		{"auth wins request code", "invalid_request_error", "invalid request", "upstream_auth_error", "", 401, 401},
		{"permission wins model code", "model_not_found", "missing model", "upstream_auth_error", "", 403, 403},
		{"rate wins request code", "invalid_request_error", "invalid request", "upstream_rate_limited", "credential_model", 429, 429},
		{"ordinary service error", "unknown_error", "internal failure", upstreamServiceKind, "request", 503, 503},
		{"transport signature retains priority", "invalid_request_error", transportIncompleteFixture, transportIncompleteKind, "request", 400, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"error": map[string]any{"code": tc.code, "message": tc.message}}
			err := classifyUpstreamResponseError(tc.httpStatus, nil, body, jsonBytes(body), time.Now())
			upstream := err.(*upstreamError)
			if upstream.Kind != tc.kind || upstream.Scope != tc.scope || upstream.Status != tc.status || upstream.HTTPStatus != tc.httpStatus || upstream.UpstreamStatus != tc.status {
				t.Fatalf("classification = %#v", upstream)
			}
			if tc.kind == upstreamRequestKind && !upstream.RetryAt.IsZero() {
				t.Fatalf("request error acquired a retry deadline: %#v", upstream)
			}
		})
	}
	for _, outer := range []int{200, 400, 502} {
		for _, protected := range []int{401, 402, 403, 429} {
			t.Run(fmt.Sprintf("nested-status-%d-%d", outer, protected), func(t *testing.T) {
				body := map[string]any{"error": map[string]any{"code": "invalid_request_error", "status": fmt.Sprint(protected), "message": "invalid request"}}
				upstream := classifyUpstreamResponseError(outer, nil, body, jsonBytes(body), time.Now()).(*upstreamError)
				if upstream.Status != protected || upstream.HTTPStatus != outer || upstream.Kind == upstreamRequestKind || upstream.Kind == upstreamModelKind {
					t.Fatalf("protected nested status lost priority: %#v", upstream)
				}
			})
		}
	}
}

func TestUpstreamResponseErrorChallengeAndRouteClassification(t *testing.T) {
	for _, tc := range []struct {
		name, raw, contentType, mitigation, kind, scope string
		httpStatus, status                              int
	}{
		{"HTML route", "<!doctype html><html><h1>Not Found</h1></html>", "text/html", "", upstreamRouteKind, "request", 404, 502},
		{"plain route", "404 page not found", "text/plain", "", upstreamRouteKind, "request", 404, 502},
		{"sniffed route", "route not found", "", "", upstreamRouteKind, "request", 404, 502},
		{"JSON model", `{"error":{"code":"model_not_found","message":"unknown model"}}`, "text/html", "", upstreamModelKind, "credential_model", 404, 404},
		{"unknown JSON 404", `{"error":{"message":"not found"}}`, "application/json", "", "upstream_error", "", 404, 404},
		{"mitigation header", "edge challenge", "text/plain", "challenge", upstreamEdgeKind, "egress_ip", 403, 502},
		{"challenge HTML", `<!doctype html><html><title>Just a moment...</title><script src="/cdn-cgi/challenge-platform/h/g/orchestrate/chl_page/v1"></script></html>`, "text/html", "", upstreamEdgeKind, "egress_ip", 403, 502},
		{"challenge options", `<html><script>window._cf_chl_opt = {};</script></html>`, "text/html", "", upstreamEdgeKind, "egress_ip", 403, 502},
		{"ordinary HTML permission", "<html><h1>Forbidden</h1></html>", "text/html", "", "upstream_auth_error", "", 403, 403},
		{"cloudflare server alone", "<html><p>Forbidden by Cloudflare</p></html>", "text/html", "", "upstream_auth_error", "", 403, 403},
		{"JSON auth beats challenge", `{"error":{"code":"invalid_api_key","message":"invalid key; /cdn-cgi/challenge-platform/"}}`, "text/html", "challenge", "upstream_auth_error", "", 403, 403},
		{"JSON quota beats challenge", `{"error":{"code":"insufficient_quota","message":"quota exhausted"}}`, "text/html", "challenge", "upstream_auth_error", "", 403, 403},
		{"top level quota beats challenge", `{"code":"insufficient_quota","message":"quota exhausted"}`, "text/html", "challenge", "upstream_auth_error", "", 403, 403},
		{"unknown JSON error stays conservative", `{"error":{"message":"forbidden"}}`, "text/html", "challenge", "upstream_auth_error", "", 403, 403},
		{"JSON team beats challenge", `{"error":{"status":429,"message":"This team's limit is 100 tokens per minute"}}`, "text/html", "challenge", "upstream_auth_error", "", 403, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.raw)
			body, _ := decodeObject(raw)
			headers := http.Header{"Content-Type": {tc.contentType}, "Cf-Mitigated": {tc.mitigation}}
			upstream := classifyUpstreamResponseError(tc.httpStatus, headers, body, raw, time.Now()).(*upstreamError)
			if upstream.Kind != tc.kind || upstream.Scope != tc.scope || upstream.Status != tc.status || upstream.HTTPStatus != tc.httpStatus || upstream.UpstreamStatus != tc.httpStatus {
				t.Fatalf("classification = %#v", upstream)
			}
			if !upstream.RetryAt.IsZero() {
				t.Fatalf("route/challenge response acquired a retry deadline: %#v", upstream)
			}
		})
	}
}

func TestUpstreamResponseErrorStructuredTypes(t *testing.T) {
	for _, tc := range []struct {
		name, kind, scope string
		body              map[string]any
	}{
		{"nested request type", upstreamRequestKind, "request", map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "bad messages"}}},
		{"top level request type", upstreamRequestKind, "request", map[string]any{"type": "context_length_exceeded", "error": "context too long"}},
		{"nested model type", upstreamModelKind, "credential_model", map[string]any{"error": map[string]any{"type": "model_not_supported", "message": "unsupported model"}}},
		{"top level model code", upstreamModelKind, "credential_model", map[string]any{"code": "model_not_found", "error": "missing model"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := jsonBytes(tc.body)
			// A nil decoded body still allows classification from a valid raw JSON
			// response; the transport status remains a separate diagnostic fact.
			upstream := classifyUpstreamResponseError(502, nil, nil, raw, time.Now()).(*upstreamError)
			if upstream.Kind != tc.kind || upstream.Scope != tc.scope || upstream.Status != 502 || upstream.HTTPStatus != 502 || upstream.UpstreamStatus != 502 {
				t.Fatalf("structured classification = %#v", upstream)
			}
		})
	}
}

func TestUpstreamResponseErrorHostReadPaths(t *testing.T) {
	for _, tc := range []struct {
		name, raw, kind string
		status          int
		header          http.Header
	}{
		{"route", "<html>404 Not Found</html>", upstreamRouteKind, 404, http.Header{"Content-Type": {"text/html"}}},
		{"challenge", "<html>challenge</html>", upstreamEdgeKind, 403, http.Header{"Cf-Mitigated": {"challenge"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService()
			host := newFakeHost(hostPlan{status: tc.status, header: tc.header, chunks: []readChunk{{Payload: []byte(tc.raw), Done: true}}})
			s.SetHost(host.call)
			up, err := s.openUpstream(map[string]any{"body": []byte(`{}`)}, time.Now().Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if up.local != nil {
				t.Fatal("test must exercise the host callback without the local egress pool")
			}
			_, err = s.readJSON(up)
			var upstream *upstreamError
			if !errors.As(err, &upstream) || upstream.Kind != tc.kind || upstream.Status != 502 || upstream.HTTPStatus != tc.status || upstream.UpstreamStatus != tc.status {
				t.Fatalf("host response classification = %#v", err)
			}
		})
	}
}
