package quarkapi

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Delete reliability policy.
//
// A destructive delete is never replayed blindly: an error response or a
// dropped connection may still mean the delete landed. Before any retry, the
// FID — the immutable object identity — is resolved through /file/info, and
// only the exact not-found signature counts as "already gone" (issue #2).
const deleteMaxAttempts = 3

// Backoff durations are variables rather than constants so tests can shrink
// them; the values are production policy and are not configurable at runtime.
var (
	deleteInitialBackoff = 250 * time.Millisecond
	deleteMaxBackoff     = 500 * time.Millisecond
)

// DeleteReliable deletes one FID, handling the ambiguous outcomes a raw
// /file/delete cannot distinguish on its own.
//
// The flow per attempt:
//
//  1. POST /file/delete exactly once (the raw op has no automatic replay).
//  2. On success, done.
//  3. On an uncertain failure — a transport error, a provider transient such
//     as "inner error, requestId ...", or a 5xx/429-class response — resolve
//     the FID first. If the provider answers with the exact not-found
//     signature, the delete has landed and the error is a false failure.
//  4. A transport-ambiguous delete whose verification also failed is reported
//     as ambiguous rather than retried: without a verifier result, a replay
//     would be unsafe.
//  5. A permanent rejection (auth, permission, invalid request) is returned
//     unchanged.
//  6. An uncertain failure with a successful "still exists" verification is
//     retried, at most deleteMaxAttempts times with capped backoff.
func (c *Client) DeleteReliable(ctx context.Context, fid string) error {
	backoff := deleteInitialBackoff
	hadTransient := false
	for attempt := 1; attempt <= deleteMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := c.Delete(ctx, DeleteRequest{FileIDs: []string{fid}})
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		transportAmbiguous := isAmbiguousTransport(err)
		providerTransient := isRetryableDeleteMessage(err)
		uncertain := transportAmbiguous || providerTransient
		if uncertain {
			hadTransient = true
		}

		// An uncertain outcome — or any outcome after an earlier uncertain
		// one — is verified before replaying the destructive request, so an
		// already-completed delete is not turned back into a failure.
		if uncertain || hadTransient {
			exists, verifyErr := c.FIDExists(ctx, fid)
			if verifyErr == nil && !exists {
				// The FID is gone under its exact not-found signature: the
				// delete landed even though the response said otherwise.
				return nil
			}
			if verifyErr != nil && transportAmbiguous {
				// The delete's outcome is unknown and the verifier could not
				// resolve it. Fail closed instead of replaying.
				return fmt.Errorf("quark delete outcome is ambiguous and fid verification failed: %w", verifyErr)
			}
		}

		if !uncertain {
			return err
		}
		if attempt == deleteMaxAttempts {
			return fmt.Errorf("quark delete transient provider error after %d attempts: %w", attempt, err)
		}

		if err := waitCtx(ctx, backoff); err != nil {
			return err
		}
		backoff = nextBackoff(backoff, deleteMaxBackoff)
	}
	return fmt.Errorf("quark delete retry loop exhausted unexpectedly")
}

// isAmbiguousTransport reports whether err is a transport failure returned by
// net/http. The request may have reached the provider, so its outcome is
// unknown. Caller cancellation is excluded by the ctx.Err checks in the loop
// above.
func isAmbiguousTransport(err error) bool {
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

// isRetryableDeleteMessage reports whether err carries the one provider
// message that is safe to replay: "inner error, requestId ...". The match is
// structural (on the preserved APIError message) and token-based, because a
// substring match would also catch OSS error bodies whose request already
// landed.
func isRetryableDeleteMessage(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(apiErr.Message))
	return hasTokenPrefix(msg, retryTokenInnerError) && strings.Contains(msg, "requestid")
}
