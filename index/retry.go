package index

import (
	"fmt"
	"time"
)

const (
	defaultBackgroundRetryInitialDelay = time.Second
	defaultBackgroundRetryMaxDelay     = time.Minute
)

// backgroundRetry keeps retrying failed work with exponential backoff up to
// maxDelay. Success resets the delay; new snapshots alone do not. It is owned
// by a single background loop.
type backgroundRetry struct {
	initialDelay time.Duration
	maxDelay     time.Duration
	delay        time.Duration
	err          error
	timer        *time.Timer
}

func newBackgroundRetry(config Config) backgroundRetry {
	return backgroundRetry{
		initialDelay: config.BackgroundRetryInitialDelay,
		maxDelay:     config.BackgroundRetryMaxDelay,
	}
}

func (r *backgroundRetry) fail(operation string, err error) error {
	r.stop()
	if r.delay == 0 {
		r.delay = r.initialDelay
	} else if r.delay >= r.maxDelay-r.delay {
		// Avoid overflowing time.Duration even for very large configured delays.
		r.delay = r.maxDelay
	} else {
		r.delay *= 2
	}
	r.timer = time.NewTimer(r.delay)
	r.err = fmt.Errorf("%s: retry in %s: %w", operation, r.delay, err)
	return r.err
}

// notifications disables work during backoff without blocking the loop's
// control channels. In particular, the persister must keep accepting merger
// subscriptions while waiting, so shutdown cannot get stuck behind a timer.
func (r *backgroundRetry) notifications(work notificationChan) (notificationChan, <-chan time.Time) {
	if r.timer != nil {
		return nil, r.timer.C
	}
	return work, nil
}

func (r *backgroundRetry) stop() {
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
}

func (r *backgroundRetry) reset() {
	r.stop()
	r.delay = 0
	r.err = nil
}
