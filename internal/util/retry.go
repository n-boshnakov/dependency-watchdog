package util

import (
	"context"
	"time"
)

type RetryResult[T any] struct {
	Value T
	Err   error
}

func Retry[T any](ctx context.Context, operation string, fn func() (T, error), numAttempts int, backOff time.Duration, canRetry func(error) bool) RetryResult[T] {
	var result T
	var err error
	for i := 1; i <= numAttempts; i++ {
		select {
		case <-ctx.Done():
			logger.Error(ctx.Err(), "context has been cancelled, stopping retry", "operation", operation)
			return RetryResult[T]{Err: ctx.Err()}
		default:
		}
		result, err = fn()
		if err == nil {
			return RetryResult[T]{Value: result, Err: err}
		}
		if !canRetry(err) {
			logger.Error(err, "exiting retry as canRetry has returned false", "operation", operation, "exitOnAttempt", i)
			return RetryResult[T]{Err: err}
		}
		select {
		case <-ctx.Done():
			logger.Error(ctx.Err(), "context has been cancelled, stopping retry", "operation", operation)
			return RetryResult[T]{Err: ctx.Err()}
		case <-time.After(backOff):
			logger.V(4).Info("will attempt to retry operation", "operation", operation, "currentAttempt", i, "error", err)
		}
	}
	return RetryResult[T]{Value: result, Err: err}
}

func RetryUntilPredicate(ctx context.Context, operation string, predicateFn func() bool, timeout time.Duration, interval time.Duration) bool {
	timer := time.NewTimer(timeout)
	for {
		select {
		case <-ctx.Done():
			logger.V(4).Info("context has been cancelled, exiting retrying operation", "operation", operation)
			return false
		case <-timer.C:
			logger.V(4).Info("timed out waiting for predicateFn to be true", "operation", operation)
			return false
		default:
			if predicateFn() {
				return true
			}
			time.Sleep(interval)
		}
	}
}

// RetryOnError retries invoking a function till either the invocation of the function does not return an error or the
// context has timed-out or has been cancelled. The consumers should ensure that the context passed to it
// has a proper finite timeout set as there is no other timeout taken as a function argument.
func RetryOnError(ctx context.Context, operation string, retriableFn func() error, interval time.Duration) {
	for {
		select {
		case <-ctx.Done():
			logger.V(4).Info("context has either timed-out or has been cancelled", "operation", operation)
			return
		default:
			err := retriableFn()
			if err != nil {
				logger.Error(err, "Error encountered during retry. Will re-attempt if possible.", "operation", operation)
				time.Sleep(interval)
				continue
			}
			return
		}
	}
}

func AlwaysRetry(err error) bool {
	return true
}