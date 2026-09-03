package clobclient

import "github.com/Cyvadra/polymarket-clob-client/internal/transport"

// APIError is returned for non-successful CLOB HTTP responses. It exposes the
// server status, the exact request path, and a bounded response body.
type APIError = transport.HTTPError
