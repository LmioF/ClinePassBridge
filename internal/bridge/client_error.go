package bridge

import (
	"errors"
	"fmt"
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
	if errors.As(err, &upstream) && upstream.IsRequestScoped() {
		switch upstream.Kind {
		case upstreamServiceKind:
			return fmt.Sprintf("HTTP %d: upstream service failed; retry the request (%s)", upstream.Status, upstream.Kind)
		case upstreamProtocolKind:
			return "HTTP 502: upstream returned an incomplete or invalid response (upstream_protocol_error)"
		case upstreamNetworkKind, upstreamCallbackKind:
			return "HTTP 502: upstream connection interrupted (" + upstream.Kind + ")"
		}
	}
	return safeError(err)
}

// HostErrorDetails preserves stock CPA's transport classification across its
// stream and RPC ABIs. This is independent of client retry normalization:
// disabling it keeps diagnostic text where the host can safely classify it.
// Only failures classified at their source receive a cooldown exemption.
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
	if !errors.As(err, &upstream) {
		return message, status
	}
	if !upstream.IsRequestScoped() {
		return protectedHostMessage(upstream, message), status
	}
	var marker string
	switch upstream.Kind {
	case transportIncompleteKind:
		marker = "unexpected EOF"
	case upstreamStreamIdleTimeoutKind:
		marker = "i/o timeout"
	case upstreamNetworkKind, upstreamCallbackKind:
		marker = "[clinepassbridge:cpa_compat:connection aborted]"
	case upstreamServiceKind, upstreamProtocolKind, bridgeUnavailableKind:
		marker = "[clinepassbridge:cpa_compat:server misbehaving]"
	case requestCanceledKind:
		return "context canceled", 0
	case requestDeadlineKind:
		return "context deadline exceeded", 0
	case responseLimitKind:
		return responseLimitHostMessage(safeError(err)), 413
	default:
		return message, status
	}
	if !strings.Contains(strings.ToLower(message), strings.ToLower(marker)) {
		message += ": " + marker
	}
	return message, 0
}
