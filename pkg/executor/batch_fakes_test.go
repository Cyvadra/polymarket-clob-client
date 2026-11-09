package executor

import (
	"context"
	"errors"
	"net/http"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
)

// The fakes answer a batch the way the exchange does, from their own single
// order behaviour: each order is posted or cancelled as though alone, a
// rejection becomes that order's success=false entry and a refusal its
// not_canceled entry, and any other error fails the whole call.

func emulateSubmits(ctx context.Context, single func(context.Context, clobclient.SignedOrderV2, clobclient.OrderType, bool) (*clobclient.OrderResponse, error), orders []clobclient.BatchOrder) ([]clobclient.OrderResponse, error) {
	out := make([]clobclient.OrderResponse, len(orders))
	for i, order := range orders {
		resp, err := single(ctx, order.Signed, order.OrderType, order.PostOnly)
		var rejected *clobclient.OrderRejectedError
		switch {
		case errors.As(err, &rejected):
			// As the exchange answers it: success=true, no order ID,
			// the refusal only in errorMsg (cmd/batchprobe, 2026-11-09).
			out[i] = clobclient.OrderResponse{Success: true, ErrorMsg: rejected.Message}
		case err != nil:
			return nil, err
		case resp != nil:
			out[i] = *resp
			out[i].Success = true
		}
	}
	return out, nil
}

func emulateCancels(ctx context.Context, single func(context.Context, string) error, ids []string) (*clobclient.CancelResult, error) {
	out := &clobclient.CancelResult{NotCanceled: map[string]string{}}
	for _, id := range ids {
		err := single(ctx, id)
		var refused *clobclient.CancelRefusedError
		switch {
		case errors.As(err, &refused):
			out.NotCanceled[id] = refused.Reason
		case err != nil:
			return nil, err
		default:
			out.Canceled = append(out.Canceled, id)
		}
	}
	return out, nil
}

func (c *fakeCLOB) SubmitSignedOrders(ctx context.Context, orders []clobclient.BatchOrder) ([]clobclient.OrderResponse, error) {
	c.batchSubmits++
	return emulateSubmits(ctx, c.SubmitSignedOrder, orders)
}

func (c *fakeCLOB) CancelOrdersResult(ctx context.Context, ids []string) (*clobclient.CancelResult, error) {
	c.batchCancels = append(c.batchCancels, len(ids))
	return emulateCancels(ctx, c.CancelOrder, ids)
}

func (c *cancelProbeCLOB) CancelOrdersResult(ctx context.Context, ids []string) (*clobclient.CancelResult, error) {
	return emulateCancels(ctx, c.CancelOrder, ids)
}

func (c *blockingCLOB) CancelOrdersResult(ctx context.Context, ids []string) (*clobclient.CancelResult, error) {
	return emulateCancels(ctx, c.CancelOrder, ids)
}

func (c *ladderCLOB) SubmitSignedOrders(ctx context.Context, orders []clobclient.BatchOrder) ([]clobclient.OrderResponse, error) {
	c.mu.Lock()
	c.batchPosts = append(c.batchPosts, len(orders))
	unsupported := c.noBatch
	c.mu.Unlock()
	if unsupported {
		return nil, &clobclient.APIError{StatusCode: http.StatusNotFound, Method: http.MethodPost, Path: "/orders"}
	}
	return emulateSubmits(ctx, c.SubmitSignedOrder, orders)
}

func (c *ladderCLOB) CancelOrdersResult(ctx context.Context, ids []string) (*clobclient.CancelResult, error) {
	c.mu.Lock()
	c.batchCancels = append(c.batchCancels, len(ids))
	unsupported := c.noBatch
	c.mu.Unlock()
	if unsupported {
		return nil, &clobclient.APIError{StatusCode: http.StatusNotFound, Method: http.MethodDelete, Path: "/orders"}
	}
	return emulateCancels(ctx, c.CancelOrder, ids)
}
