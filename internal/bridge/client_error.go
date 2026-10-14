package bridge

import (
	"errors"
	"net/http"
	"strings"
)

// ClientErrorMessage applies the current compatibility setting at the client
// boundary; diagnostic logs keep the original error.
func (s *Service) ClientErrorMessage(err error) string {
	if s != nil && !s.config().ClientRetryCompatibility {
		return safeError(err)
	}
	return ClientErrorMessage(err)
}

// ClientErrorMessage separates a known transport fault from nested provider
// exception codes. Text-classifying clients otherwise mistake the internal
// TransferEncodingError's 400 for a rejected request, even after an HTTP 502.
// The original exception remains on the error and in the diagnostic log.
func ClientErrorMessage(err error) string {
	var upstream *upstreamError
	if errors.As(err, &upstream) && upstream.Kind == transportIncompleteKind && upstream.IsRequestScoped() {
		return "HTTP 502: upstream response stream ended before completion: unexpected EOF (upstream_transport_incomplete)"
	}
	return safeError(err)
}

// HostErrorDetails preserves stock CPA's transport classification across its
// stream and RPC ABIs. This is independent of client retry normalization:
// disabling that option keeps the original diagnostic text plus the EOF marker.
// Only these positively classified request faults receive a cooldown exemption.
// A zero status is an absent RPC status, not an HTTP response status: stock CPA
// recognizes the transport message before rendering a normal 500 client error.
func (s *Service) HostErrorDetails(err error) (string, int) {
	message := s.ClientErrorMessage(err)
	status := http.StatusInternalServerError
	var coded interface{ StatusCode() int }
	if errors.As(err, &coded) && coded.StatusCode() >= 400 && coded.StatusCode() <= 599 {
		status = coded.StatusCode()
	}
	var upstream *upstreamError
	if !errors.As(err, &upstream) || !upstream.IsRequestScoped() {
		return message, status
	}
	var marker string
	switch upstream.Kind {
	case transportIncompleteKind:
		marker = "unexpected EOF"
	case upstreamStreamIdleTimeoutKind:
		marker = "i/o timeout"
	default:
		return message, status
	}
	if !strings.Contains(strings.ToLower(message), strings.ToLower(marker)) {
		message += ": " + marker
	}
	return message, 0
}
