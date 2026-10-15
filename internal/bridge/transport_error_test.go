package bridge

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHTTPCallbackTransportFaultsStayRequestScoped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		kind   string
		status int
	}{
		{"reset", syscall.ECONNRESET, upstreamNetworkKind, 502},
		{"eof", io.ErrUnexpectedEOF, upstreamNetworkKind, 502},
		{"dns", &net.DNSError{Err: "fixture"}, upstreamNetworkKind, 502},
		{"cancel", context.Canceled, requestCanceledKind, 499},
		{"deadline", context.DeadlineExceeded, requestDeadlineKind, 504},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &url.Error{Op: "Post", URL: "https://fixture-user:fixture-secret@example.invalid", Err: tc.err}
			fault := hostHTTPFault(err, nil, "upstream transport failed")
			var upstream *upstreamError
			if !errors.As(fault, &upstream) || upstream.Kind != tc.kind || upstream.Status != tc.status || !upstream.IsRequestScoped() {
				t.Fatalf("fault = %#v", fault)
			}
			if strings.Contains(fault.Error(), "fixture-secret") {
				t.Fatal("transport error leaked credentials")
			}
		})
	}
	if fault := hostHTTPFault(errors.New("connection reset; deadline exceeded"), nil, "upstream transport failed"); fault.(*upstreamError).Kind != upstreamCallbackKind {
		t.Fatal("legacy callback lost its callback source classification")
	}
}

func TestHTTPCallbackWireMetadataPreservesResponseStatus(t *testing.T) {
	for _, status := range []int{200, 401, 402, 403, 429, 502, 504} {
		s := registeredService(t, "native")
		h := newFakeHost(hostPlan{status: status, header: http.Header{"Content-Type": []string{"application/json"}}})
		s.SetHost(func(method string, request, response any) error {
			if method == "host.http.stream_read" {
				*response.(*readChunk) = readChunk{Payload: []byte(`{"error":{"message":"fixture upstream failure"}}`), Done: true, Error: "host read failed", ErrorDetails: &httpErrorDetails{Kind: "connection_reset", Code: upstreamNetworkKind, HTTPStatus: 502}}
				return nil
			}
			return h.call(method, request, response)
		})
		stream, err := s.openUpstream(map[string]any{"body": []byte(`{}`)}, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.readJSON(stream)
		if status == 200 {
			if upstream, ok := err.(*upstreamError); !ok || !upstream.IsRequestScoped() || upstream.Kind != upstreamNetworkKind {
				t.Fatalf("typed read fault = %v", err)
			}
		} else if upstream, ok := err.(*upstreamError); !ok || upstream.Status != status || upstream.IsRequestScoped() != (status >= 500) {
			t.Fatalf("HTTP %d lost its response policy: %v", status, err)
		} else if status >= 500 && upstream.Kind != upstreamServiceKind {
			t.Fatalf("HTTP %d became a callback fault: %v", status, err)
		}
	}
}

func TestLegacyCallbackTerminationRequiresExactBoundary(t *testing.T) {
	for _, tc := range []struct {
		message, kind string
		status        int
	}{
		{"context canceled", requestCanceledKind, 499},
		{"host read: context canceled", requestCanceledKind, 499},
		{"context deadline exceeded", requestDeadlineKind, 504},
		{"host read: context deadline exceeded", requestDeadlineKind, 504},
		{"provider says context canceled during validation", upstreamCallbackKind, 502},
		{"context deadline exceeded; invalid API key", upstreamCallbackKind, 502},
		{"context canceled by provider", upstreamCallbackKind, 502},
		{"HTTP/2 INTERNAL_ERROR", upstreamCallbackKind, 502},
	} {
		t.Run(tc.message, func(t *testing.T) {
			fault := hostHTTPFault(errors.New(tc.message), nil, "upstream stream interrupted")
			upstream, ok := fault.(*upstreamError)
			if !ok || upstream.Kind != tc.kind || upstream.Status != tc.status || !upstream.IsRequestScoped() {
				t.Fatalf("legacy callback = %#v", fault)
			}
		})
	}
	for _, status := range []int{401, 402, 403, 429, 502} {
		fault := hostHTTPFault(errorPolicyStatusError{message: "context canceled", status: status}, nil, "upstream stream interrupted")
		if statusOf(fault) != status {
			t.Fatalf("typed HTTP %d lost precedence: %v", status, fault)
		}
		if upstream, ok := fault.(*upstreamError); ok && upstream.IsRequestScoped() {
			t.Fatalf("typed HTTP %d acquired callback exemption: %v", status, fault)
		}
	}
}
