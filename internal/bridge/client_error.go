package bridge

import "errors"

// ClientErrorMessage applies the current compatibility setting at the client
// boundary; diagnostic logs and account scheduling keep the original error.
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
		return "HTTP 502: upstream response stream ended before completion (upstream_transport_incomplete)"
	}
	return safeError(err)
}
