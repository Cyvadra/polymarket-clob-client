package clobclient

import (
	"context"
	"fmt"
	"net/http"

	"github.com/Cyvadra/polymarket-clob-client/internal/transport"
)

// Heartbeat sends one POST /v1/heartbeats, the CLOB's dead man's switch: once
// a session has started, the exchange cancels every open order of the
// account if heartbeats stop arriving. Pass "" to start a session, then the
// returned ID each time; a stale ID is refused with 400. The exchange does not
// document the timeout, so nothing in this repository sends heartbeats yet:
// a loop needs that timeout measured, and a plan for the gap a restart
// leaves, before it can be trusted not to cancel resting orders.
func (c *Client) Heartbeat(ctx context.Context, heartbeatID string) (string, error) {
	if c.cfg.Credentials == nil {
		return "", fmt.Errorf("API credentials are required to send a heartbeat")
	}
	var out struct {
		HeartbeatID string `json:"heartbeat_id"`
	}
	body := map[string]string{"heartbeat_id": heartbeatID}
	if err := c.transport.Do(ctx, transport.Request{Method: http.MethodPost, Path: "/v1/heartbeats", Body: body, Headers: c.l2Headers}, &out); err != nil {
		return "", err
	}
	if out.HeartbeatID == "" {
		return "", fmt.Errorf("heartbeat response carries no heartbeat_id")
	}
	return out.HeartbeatID, nil
}
