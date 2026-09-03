// Package poll provides context-aware polling for internal API workflows.
package poll

import (
	"context"
	"time"
)

// Until invokes observe until it reports completion or an error occurs.
func Until(ctx context.Context, interval time.Duration, observe func() (bool, error)) error {
	for {
		complete, err := observe()
		if err != nil || complete {
			return err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
