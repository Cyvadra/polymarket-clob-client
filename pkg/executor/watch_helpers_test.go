package executor

import "context"

// checkWatched runs one deadline pass and waits for its cancels, so a test
// can assert on their outcome. Production runs watchCancelPass, which does
// not wait; cancel_test covers that path.
func (e *Executor) checkWatched(ctx context.Context) error {
	return <-e.startWatchedCancels(ctx)
}
