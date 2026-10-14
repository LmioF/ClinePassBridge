package bridge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/url"
	"regexp"
	"syscall"
)

const upstreamNetworkKind = "upstream_network_error"

var transportURLPattern = regexp.MustCompile(`(?i)\b(?:https?|socks5h?)://[^\s<>"']+`)

// Legacy hosts provide only text. Keep useful diagnostics without promoting
// text to a typed fault, and remove URL credentials/query/path fragments.
func legacyTransportMessage(err error, message string) string {
	if err == nil {
		return message
	}
	return message + ": " + safeError(errors.New(legacyTransportDetail(err)))
}

func legacyTransportDetail(err error) string {
	if err == nil {
		return ""
	}
	return transportURLPattern.ReplaceAllStringFunc(secretPattern.ReplaceAllString(err.Error(), "[REDACTED]"), func(raw string) string {
		u, parseErr := url.Parse(raw)
		if parseErr != nil || u.Host == "" {
			return "[REDACTED URL]"
		}
		return u.Scheme + "://" + u.Host
	})
}

func callbackTransportDiagnostic(err error, details *httpErrorDetails, fault error) string {
	var api *APIError
	if details == nil && errors.As(fault, &api) && api.Kind == "upstream_error" {
		return legacyTransportDetail(err)
	}
	return fault.Error()
}

// Matches pluginapi.HostHTTPErrorDetails without coupling this plugin to the
// host SDK. HTTPStatus is the callback fault status, not a received HTTP status.
type httpErrorDetails struct {
	Kind       string `json:"kind"`
	Code       string `json:"code"`
	HTTPStatus int    `json:"http_status"`
}

func upstreamNetworkFailure(kind, message string) error {
	return &upstreamError{
		APIError: &APIError{Status: 502, Kind: upstreamNetworkKind, Message: "HTTP 502: " + message + " (" + kind + ")"},
		Scope:    "request",
	}
}

func networkFaultKind(kind string) bool {
	switch kind {
	case "eof", "connection_reset", "connection_closed", "dns", "tls", "timeout", "network":
		return true
	default:
		return false
	}
}

// Stable code/status pairs are accepted only from structured callback metadata
// or a typed error. Legacy callback strings never establish exemption.
func structuredHTTPFault(details *httpErrorDetails, message string) error {
	if details == nil {
		return nil
	}
	switch {
	case details.Code == requestCanceledKind && details.HTTPStatus == 499 && details.Kind == "canceled":
		return requestCanceled(message + " (canceled)")
	case details.Code == requestDeadlineKind && details.HTTPStatus == 504 && details.Kind == "deadline":
		return requestTimedOut(message + " (deadline)")
	case details.Code == upstreamNetworkKind && details.HTTPStatus == 502 && networkFaultKind(details.Kind):
		return upstreamNetworkFailure(details.Kind, message)
	default:
		return nil
	}
}

func hostHTTPFault(err error, details *httpErrorDetails, message string) error {
	if fault := structuredHTTPFault(details, message); fault != nil {
		return fault
	}
	if err != nil {
		var coded interface{ Code() string }
		var status interface{ StatusCode() int }
		if errors.As(err, &coded) && errors.As(err, &status) {
			kind := ""
			switch coded.Code() {
			case requestCanceledKind:
				kind = "canceled"
			case requestDeadlineKind:
				kind = "deadline"
			case upstreamNetworkKind:
				kind = "network"
			}
			if fault := structuredHTTPFault(&httpErrorDetails{Kind: kind, Code: coded.Code(), HTTPStatus: status.StatusCode()}, message); fault != nil {
				return fault
			}
		}
	}
	return typedTransportFault(nil, err, message)
}

// The source error's text may contain proxy credentials or a request URL.
// Use its type/cause for classification and a bounded fixed source label only.
func typedTransportFault(ctx context.Context, err error, message string) error {
	var status interface{ StatusCode() int }
	if errors.As(err, &status) && status.StatusCode() >= 400 && status.StatusCode() <= 599 {
		return fail(status.StatusCode(), message)
	}
	if errors.Is(err, context.DeadlineExceeded) || (ctx != nil && errors.Is(context.Cause(ctx), context.DeadlineExceeded)) {
		return requestTimedOut(message + " (deadline)")
	}
	if errors.Is(err, context.Canceled) || (ctx != nil && errors.Is(ctx.Err(), context.Canceled)) {
		return requestCanceled(message + " (canceled)")
	}
	kind := ""
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		kind = "eof"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.ECONNABORTED), errors.Is(err, syscall.EPIPE):
		kind = "connection_reset"
	case errors.Is(err, net.ErrClosed), errors.Is(err, io.ErrClosedPipe):
		kind = "connection_closed"
	default:
		var dns *net.DNSError
		var certificate *tls.CertificateVerificationError
		var authority x509.UnknownAuthorityError
		var invalidCertificate x509.CertificateInvalidError
		var hostname x509.HostnameError
		var record tls.RecordHeaderError
		var alert tls.AlertError
		var timeout interface{ Timeout() bool }
		var operation *net.OpError
		switch {
		case errors.As(err, &dns):
			kind = "dns"
		case errors.As(err, &certificate), errors.As(err, &authority), errors.As(err, &invalidCertificate), errors.As(err, &hostname), errors.As(err, &record), errors.As(err, &alert):
			kind = "tls"
		case errors.As(err, &timeout) && timeout.Timeout():
			kind = "timeout"
		case errors.As(err, &operation), errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
			kind = "network"
		}
	}
	if kind != "" {
		return upstreamNetworkFailure(kind, message)
	}
	return fail(502, legacyTransportMessage(err, message))
}
