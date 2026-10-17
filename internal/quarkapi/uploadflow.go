package quarkapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/muou000/quark-rclone/internal/quarkerr"
)

// The upload flow, and why each stage has the retry policy it has:
//
//	pre      single attempt, no redirect  — a lost response may have allocated
//	                                        a task and a FID
//	hash     control-plane retry          — no body, no side effect beyond dedup
//	part     auth retry only              — the OSS PUT is not replayable once
//	                                        bytes are in flight
//	commit   control-plane retry          — signs and assembles what exists
//	finish   control-plane retry + a
//	         visibility tiebreaker        — the FID may already be visible even
//	                                        though the call keeps failing
//
// The distinction between "control-plane transient" and "OSS error" is the
// whole safety story. OSS error bodies also contain the text "inner error,
// requestId ...", so a substring match would happily replay a request whose
// body already landed on the server.

// Upload uploads one file to Quark and returns the FID assigned to it.
//
// The body is read sequentially in parts of the size the provider asked for,
// so memory stays bounded to one part no matter how large the file is.
// onProgress, when non-nil, is called after each part with the running byte
// total.
func (c *Client) Upload(ctx context.Context, req UploadRequest, body io.Reader, onProgress func(sent int64)) (string, error) {
	if req.Size < 0 {
		return "", quarkerr.New(quarkerr.CatInvalidRequest, "upload", "negative size")
	}

	// PRE is outside the retry helper on purpose.
	session, err := c.preUpload(ctx, req)
	if err != nil {
		return "", err
	}
	fid := session.Task.FID

	// A file that Quark already holds needs no bytes at all.
	if req.MD5 != "" && req.SHA1 != "" {
		finish, err := retryControl(ctx, "hash", func() (bool, error) {
			return c.offerHash(ctx, req.MD5, req.SHA1, session.Task.TaskID)
		})
		if err != nil {
			return "", err
		}
		if finish {
			return fid, nil
		}
	}

	partSize := session.Task.PartSize
	if partSize <= 0 {
		partSize = DefaultUploadPartSize
	}

	etags := make([]string, 0, (req.Size+partSize-1)/partSize)
	buf := make([]byte, partSize)
	var sent int64
	// The condition runs before each iteration, so a zero-byte file simply
	// uploads no parts: it still needs PRE and finish, but there is nothing to
	// send and an empty OSS multipart upload would be rejected.
	for partNumber := 1; sent < req.Size; partNumber++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		want := partSize
		if remaining := req.Size - sent; remaining < want {
			want = remaining
		}
		chunk := buf[:want]
		n, err := readFull(body, chunk)
		if err != nil {
			return "", err
		}
		chunk = chunk[:n]

		// The retry for a part lives inside uploadPart, around the signing
		// call only: the PUT itself runs at most once per part.
		etag, err := c.uploadPart(ctx, session.Task, session.ContentType, partNumber, bytes.NewReader(chunk))
		if err != nil {
			return "", err
		}
		etags = append(etags, etag)
		sent += int64(n)
		if onProgress != nil {
			onProgress(sent)
		}
	}

	if len(etags) > 0 {
		if _, err := retryControl(ctx, "commit", func() (struct{}, error) {
			return struct{}{}, c.commit(ctx, session.Task, etags)
		}); err != nil {
			return "", err
		}
	}

	// Finish is where an upload can be complete while the call keeps failing.
	// Quark allocates the FID at PRE time and the OSS callback may already
	// have fired, so the listing can show the object even though
	// /file/upload/finish returns a transient. Checking visibility first would
	// turn that uncertain finish into a false success, so it is a last resort
	// only.
	if err := c.finishReliable(ctx, session.Task, fid, req.ParentFID); err != nil {
		return "", err
	}
	return fid, nil
}

// PrepareUpload allocates an upload task without sending any bytes.
//
// It is exposed separately from Upload so a caller that must buffer the whole
// file first — which is what Quark's size-before-bytes requirement forces —
// can resolve the FID early and still drive the remaining stages itself.
func (c *Client) PrepareUpload(ctx context.Context, req UploadRequest) (*UploadSession, error) {
	return c.preUpload(ctx, req)
}

// UploadPart writes one part to the object store and returns its ETag.
func (c *Client) UploadPart(ctx context.Context, task UploadTask, contentType string, partNumber int, body io.Reader) (string, error) {
	return c.uploadPart(ctx, task, contentType, partNumber, body)
}

// CommitUpload assembles uploaded parts into one object.
func (c *Client) CommitUpload(ctx context.Context, task UploadTask, etags []string) error {
	return c.commit(ctx, task, etags)
}

// FinishUpload makes the uploaded object visible in the drive listing.
func (c *Client) FinishUpload(ctx context.Context, task UploadTask) error {
	return c.finish(ctx, task)
}

// finishReliable retries finish, then consults the drive listing as a
// tiebreaker once the retry budget is spent.
//
// The tiebreaker keys on the FID rather than the name: an older object can
// share both the name and the size, so a name match would report success for a
// file that was never uploaded.
func (c *Client) finishReliable(ctx context.Context, task UploadTask, fid, parentFID string) error {
	backoff := uploadInitialBackoff
	for attempt := 1; attempt <= uploadMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := c.finish(ctx, task)
		if err == nil {
			return nil
		}
		if !isControlTransient(err) {
			return err
		}
		if attempt < uploadMaxAttempts {
			if werr := waitCtx(ctx, backoff); werr != nil {
				return werr
			}
			backoff = nextBackoff(backoff, uploadMaxBackoff)
			continue
		}
		if fid != "" {
			visible, verr := c.visibleIn(ctx, parentFID, fid)
			if visible {
				return nil
			}
			if verr != nil {
				// The visibility probe failed for its own reason. The finish
				// failure is still the one to report, so it is not discarded.
				return errors.Join(fmt.Errorf("finish: %w", err), fmt.Errorf("visibility probe: %w", verr))
			}
		}
		return fmt.Errorf("finish: transient provider error after %d attempts: %w", attempt, err)
	}
	return fmt.Errorf("finish: retry loop exhausted unexpectedly")
}

// visibleIn reports whether a FID appears in a directory listing. It is the
// tiebreaker for an uncertain finish, so it never guesses: an absent FID is a
// definitive "not there", and a failed listing is reported separately rather
// than being read as absence.
func (c *Client) visibleIn(ctx context.Context, parentFID, fid string) (bool, error) {
	if fid == "" {
		return false, nil
	}
	backoff := uploadVisibilityInitialBackoff
	var lastErr error
	for attempt := 0; attempt < uploadVisibilityAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		entries, err := c.List(ctx, parentFID)
		if err != nil {
			lastErr = err
		} else {
			lastErr = nil
			for _, e := range entries {
				if !e.IsDir() && e.FID == fid {
					return true, nil
				}
			}
		}
		if attempt == uploadVisibilityAttempts-1 {
			break
		}
		if err := waitCtx(ctx, backoff); err != nil {
			return false, err
		}
		backoff = nextBackoff(backoff, uploadVisibilityMaxBackoff)
	}
	return false, lastErr
}

// retryControl runs a control-plane call under the bounded retry policy.
//
// It is a free function rather than a method because Go does not allow type
// parameters on methods, and the stage name is passed explicitly at every call
// site so a retry log can always be traced to the stage that produced it.
func retryControl[T any](ctx context.Context, stage string, fn func() (T, error)) (T, error) {
	var zero T
	backoff := uploadInitialBackoff
	for attempt := 1; attempt <= uploadMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		value, err := fn()
		if err == nil {
			return value, nil
		}
		if !isControlTransient(err) {
			return zero, err
		}
		if attempt == uploadMaxAttempts {
			return zero, fmt.Errorf("%s: transient provider error after %d attempts: %w", stage, attempt, err)
		}
		if werr := waitCtx(ctx, backoff); werr != nil {
			return zero, werr
		}
		backoff = nextBackoff(backoff, uploadMaxBackoff)
	}
	return zero, fmt.Errorf("%s: retry loop exhausted unexpectedly", stage)
}

// isControlTransient reports whether an error is one of the two Quark
// control-plane transients that may be replayed.
//
// The check is token-based, not substring-based. OSS error bodies contain
// "inner error, requestId ..." too, and those must not be retried here: the
// request that produced them already reached the server.
func isControlTransient(err error) bool {
	if err == nil {
		return false
	}
	// A categorised error already carries the provider's judgement, so it is
	// never re-decided from its message.
	var de *quarkerr.Error
	if errors.As(err, &de) {
		return de.Category == quarkerr.CatUpstreamTransient || de.Category == quarkerr.CatRateLimited
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if hasTokenPrefix(msg, retryTokenLockTimeout) {
		return true
	}
	return hasTokenPrefix(msg, retryTokenInnerError) && strings.Contains(msg, "requestid")
}

// hasTokenPrefix reports whether msg starts with token followed by a
// delimiter. Requiring a delimiter stops "inner errorx" from matching "inner
// error".
func hasTokenPrefix(msg, token string) bool {
	if msg == token {
		return true
	}
	if len(msg) <= len(token) || msg[:len(token)] != token {
		return false
	}
	switch msg[len(token)] {
	case ' ', ',', ':', '\t':
		return true
	}
	return false
}

// waitCtx sleeps for d unless the context ends first.
func waitCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// nextBackoff doubles d, capped at max. The cap is a parameter because each
// flow owns its own ceiling: a delete probe and an upload stage differ in how
// long they are worth waiting on.
func nextBackoff(d, max time.Duration) time.Duration {
	d *= 2
	if d > max {
		return max
	}
	return d
}

// readFull fills buf completely, tolerating a stream that ends exactly at a
// part boundary.
//
// io.ReadFull reports io.ErrUnexpectedEOF when the stream ends short of a full
// part. That is the expected end of the final part, so it is translated into
// the number of bytes actually read rather than an error; a stream that ends
// with fewer bytes than the file size promised is caught by the size check in
// the caller.
func readFull(r io.Reader, buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	n, err := io.ReadFull(r, buf)
	if err == io.ErrUnexpectedEOF {
		return n, nil
	}
	return n, err
}
