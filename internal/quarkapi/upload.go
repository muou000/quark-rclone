package quarkapi

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/muou000/quark-rclone/internal/quarkerr"
)

// Retry policy for the upload control plane.
//
// These are deliberately narrow. The only Quark messages that are safe to
// replay are two specific provider transients; everything else is either a
// permanent rejection or an OSS-level error whose retry belongs to a different
// layer.
const (
	uploadMaxAttempts        = 3
	uploadVisibilityAttempts = 4
)

// Backoff durations are variables rather than constants so tests can shrink
// them; the values are production policy and are not configurable at runtime.
var (
	uploadInitialBackoff           = 500 * time.Millisecond
	uploadMaxBackoff               = 2 * time.Second
	uploadVisibilityInitialBackoff = 250 * time.Millisecond
	uploadVisibilityMaxBackoff     = time.Second
)

// The two provider transients that may be replayed. They are matched as whole
// tokens rather than substrings, because "inner error" also appears inside OSS
// error bodies where replaying would resend a body the server already has.
var (
	retryTokenLockTimeout = "complete_upload_lock_timeout"
	retryTokenInnerError  = "inner error"
)

// ErrUploadRejected is returned when Quark refuses an upload for a reason that
// retrying cannot fix, such as a name collision. It is classified as a target
// conflict so the caller answers 409 rather than 500.
var ErrUploadRejected = errors.New("quark: upload rejected")

// UploadRequest describes one file to upload.
type UploadRequest struct {
	// ParentFID is the destination directory.
	ParentFID string
	// Name is the file name at the destination.
	Name string
	// Size is the total byte count. Quark validates it against the OSS commit,
	// so it must be exact.
	Size int64
	// ContentType is the MIME type. An empty value is sent as the provider
	// default rather than being omitted, because the field participates in
	// the OSS signature.
	ContentType string
	// MD5 and SHA1 are the whole-file digests. Quark uses them to deduplicate:
	// when the hash matches an object already in the account, no bytes are
	// sent. They are optional but strongly preferred, since supplying them
	// turns a repeat upload of an unchanged file into two requests.
	MD5  string
	SHA1 string
	// Now overrides the timestamp sent with the request. Tests use it to make
	// the payload deterministic.
	Now time.Time
}

// UploadSession carries an allocated upload task plus the resolved content
// type for the OSS part requests.
type UploadSession struct {
	// Task is the allocation from /file/upload/pre.
	Task UploadTask
	// ContentType is the resolved MIME type for the OSS part requests.
	ContentType string
}

// ErrRedirected is returned when a request that must not be replayed answers
// with a redirect.
//
// Redirects are refused rather than followed for /file/upload/pre
// specifically. A PRE that was redirected may already have allocated a task,
// and following the redirect would issue a second allocation while the caller
// believes it made one request.
var ErrRedirected = errors.New("quark: request answered with a redirect")

// preResponse is the /file/upload/pre payload.
type preResponse struct {
	envelope
	Data struct {
		TaskID    string `json:"task_id"`
		Finish    bool   `json:"finish"`
		UploadID  string `json:"upload_id"`
		ObjKey    string `json:"obj_key"`
		UploadURL string `json:"upload_url"`
		Fid       string `json:"fid"`
		Bucket    string `json:"bucket"`
		AuthInfo  string `json:"auth_info"`
		Callback  struct {
			CallbackURL  string `json:"callbackUrl"`
			CallbackBody string `json:"callbackBody"`
		} `json:"callback"`
	} `json:"data"`
	Metadata struct {
		PartSize int `json:"part_size"`
	} `json:"metadata"`
}

// hashResponse is the /file/update/hash payload.
type hashResponse struct {
	envelope
	Data struct {
		Finish bool   `json:"finish"`
		Fid    string `json:"fid"`
	} `json:"data"`
}

// authResponse is the /file/upload/auth payload.
type authResponse struct {
	envelope
	Data struct {
		AuthKey string `json:"auth_key"`
	} `json:"data"`
}

// preUpload allocates an upload task.
//
// This is the one request in the upload flow that is never retried and never
// follows a redirect. A lost response may still have allocated a task and a
// FID, so a retry would create a second task while the caller believes it made
// one request, and a followed redirect could allocate against a different host
// than the one the caller will finish against.
func (c *Client) preUpload(ctx context.Context, req UploadRequest) (*UploadSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	ct := req.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	body := map[string]any{
		"ccp_hash_update": true,
		"dir_name":        "",
		"file_name":       req.Name,
		"format_type":     ct,
		"l_created_at":    now.UnixMilli(),
		"l_updated_at":    now.UnixMilli(),
		"pdir_fid":        req.ParentFID,
		"size":            req.Size,
	}

	// The cookie is snapshotted and passed explicitly rather than read inside
	// request(), so the PRE and any later auth call cannot be signed against
	// two different values if a rotation lands between them.
	httpReq, err := c.buildUploadRequest(ctx, EndpointUploadPre, c.source.CookieSnapshot(), body)
	if err != nil {
		return nil, err
	}

	// Redirects are refused, not followed. /file/upload/pre may allocate a
	// task before its response is lost, so following a redirect could issue a
	// second allocation while the caller believes it made one request. The
	// check lives on the request rather than on the shared client so ordinary
	// API calls keep their behaviour.
	res, err := c.doNoRedirect(ctx, httpReq)
	if err != nil {
		return nil, err
	}
	raw, err := c.readBodyAndMergeRefresh(res)
	if err != nil {
		return nil, err
	}

	// A redirect on PRE is a refusal, not something to follow.
	if res.StatusCode >= 300 && res.StatusCode < 400 {
		return nil, fmt.Errorf("%w: HTTP status %d", ErrRedirected, res.StatusCode)
	}

	var env preResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, nonEnvelopeError(string(EndpointUploadPre), res.StatusCode, raw)
	}
	if res.StatusCode != http.StatusOK || env.Status >= 400 || env.Code != 0 {
		return nil, c.uploadRejection(string(EndpointUploadPre), res.StatusCode, env.Status, env.Code, env.Message)
	}

	task, err := uploadTaskFromPre(c, &env)
	if err != nil {
		return nil, err
	}
	return &UploadSession{Task: task, ContentType: ct}, nil
}

// readBodyAndMergeRefresh drains a response body and merges any observed
// session refresh fields into the credential source. It exists because the PRE
// path reads the body itself (it must inspect redirects and raw payloads), so
// it cannot go through decodeEnvelope.
func (c *Client) readBodyAndMergeRefresh(res *http.Response) ([]byte, error) {
	defer res.Body.Close()
	fields := extractRefresh(res)
	raw, err := io.ReadAll(res.Body)
	if len(fields) > 0 {
		c.source.MergeRefresh(fields)
	}
	return raw, err
}

// uploadTaskFromPre validates a PRE payload and converts it into an
// UploadTask.
//
// Validation is not defensive noise: every one of these fields is used to
// build a signed OSS request, and a missing value would produce an unsigned or
// misdirected request that fails later with an opaque OSS error. Failing here
// names the missing field instead.
//
// The client is passed in only so the host-suffix check can be skipped when a
// base override is configured; nothing else here touches it.
func uploadTaskFromPre(c *Client, env *preResponse) (UploadTask, error) {
	host := strings.TrimPrefix(env.Data.UploadURL, "https://")
	required := []struct {
		name  string
		value string
	}{
		{"task_id", env.Data.TaskID},
		{"upload_id", env.Data.UploadID},
		{"obj_key", env.Data.ObjKey},
		{"bucket", env.Data.Bucket},
		{"auth_info", env.Data.AuthInfo},
	}
	for _, f := range required {
		if f.value == "" {
			return UploadTask{}, fmt.Errorf("%w: invalid pre response: missing %s", ErrUploadRejected, f.name)
		}
	}
	// The host must be a bare host suffix. A path or query in it would mean
	// the provider changed the shape, and building a URL from it would
	// silently send the object somewhere else.
	//
	// The check is skipped when a base override is configured, because that
	// override replaces the advertised host entirely and the advertised value
	// is then never used to build a URL.
	if c.cfg.UploadURLBase == "" {
		if host == "" || strings.ContainsAny(host, "/?#") {
			return UploadTask{}, fmt.Errorf("%w: invalid pre response: upload_url is not a host suffix", ErrUploadRejected)
		}
	}

	callback, err := json.Marshal(env.Data.Callback)
	if err != nil {
		return UploadTask{}, fmt.Errorf("%w: encode callback: %v", ErrUploadRejected, err)
	}

	partSize := int64(env.Metadata.PartSize)
	if partSize <= 0 {
		partSize = DefaultUploadPartSize
	}
	return UploadTask{
		TaskID:        env.Data.TaskID,
		UploadID:      env.Data.UploadID,
		ObjKey:        env.Data.ObjKey,
		Bucket:        env.Data.Bucket,
		UploadURLHost: host,
		FID:           env.Data.Fid,
		AuthInfo:      env.Data.AuthInfo,
		Callback:      string(callback),
		PartSize:      partSize,
	}, nil
}

// ossBase returns the configured OSS base URL, if any. Tests set it to point
// the object requests at a local server; production leaves it empty so the
// provider host is used.
func (c *Client) ossBase() string { return c.cfg.UploadURLBase }

// offerHash offers whole-file digests so Quark can deduplicate.
//
// This is the cheapest possible success: when the hash matches an object
// already in the account, Quark marks the upload finished and no bytes move.
// It is retried as a control-plane call because it has no body and no side
// effect beyond the dedup decision.
func (c *Client) offerHash(ctx context.Context, md5Hex, sha1Hex, taskID string) (bool, error) {
	body := map[string]any{
		"md5":     md5Hex,
		"sha1":    sha1Hex,
		"task_id": taskID,
	}
	var resp hashResponse
	if err := c.do(ctx, EndpointUploadHash, http.MethodPost, body, &resp); err != nil {
		return false, err
	}
	return resp.Data.Finish, nil
}

// authFor signs one OSS operation and returns the Authorization value.
//
// Quark does not expose a long-lived OSS credential. Instead the caller asks
// for a signature over an exact canonical string describing the operation, and
// OSS rejects any request whose actual headers differ from what was signed.
// That is why the canonical string is assembled here from the same values that
// are then put on the request: a mismatch between the two is an OSS 403 whose
// message says nothing about the real cause.
func (c *Client) authFor(ctx context.Context, task UploadTask, canonical string) (string, error) {
	body := map[string]any{
		"auth_info": task.AuthInfo,
		"auth_meta": canonical,
		"task_id":   task.TaskID,
	}
	var resp authResponse
	if err := c.do(ctx, EndpointUploadAuth, http.MethodPost, body, &resp); err != nil {
		return "", err
	}
	if resp.Data.AuthKey == "" {
		return "", fmt.Errorf("%w: auth response carried no auth_key", ErrUploadRejected)
	}
	return resp.Data.AuthKey, nil
}

// uploadPart writes one part to OSS and returns its ETag.
//
// The reader is supplied by the caller and is consumed exactly once: a retry
// would need the bytes again, and this method deliberately does not buffer a
// whole part to make retries possible. Retrying an OSS PUT is unsafe in
// general because the request may have landed, so the retry below is scoped
// to the signing call, which happens before any bytes are sent.
func (c *Client) uploadPart(ctx context.Context, task UploadTask, contentType string, partNumber int, body io.Reader) (string, error) {
	timeStr := time.Now().UTC().Format(http.TimeFormat)
	canonical := fmt.Sprintf("PUT\n\n%s\n%s\nx-oss-date:%s\nx-oss-user-agent:%s\n/%s/%s?partNumber=%d&uploadId=%s",
		contentType, timeStr, timeStr, ossUserAgent,
		task.Bucket, task.ObjKey, partNumber, task.UploadID)

	authKey, err := retryControl(ctx, fmt.Sprintf("part[%d] auth", partNumber), func() (string, error) {
		return c.authFor(ctx, task, canonical)
	})
	if err != nil {
		return "", err
	}

	u, err := url.Parse(task.ObjectURL(c.ossBase()))
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("partNumber", strconv.Itoa(partNumber))
	q.Set("uploadId", task.UploadID)
	u.RawQuery = q.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), body)
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Authorization", authKey)
	httpReq.Header.Set("Content-Type", contentType)
	httpReq.Header.Set("Referer", c.cfg.Referer)
	httpReq.Header.Set("x-oss-date", timeStr)
	httpReq.Header.Set("x-oss-user-agent", ossUserAgent)

	res, err := c.http.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return "", fmt.Errorf("%w: part %d: OSS status %d: %s", ErrUploadRejected, partNumber, res.StatusCode, strings.TrimSpace(string(raw)))
	}
	etag := res.Header.Get("Etag")
	if etag == "" {
		return "", fmt.Errorf("%w: part %d: OSS returned no ETag", ErrUploadRejected, partNumber)
	}
	return etag, nil
}

// commit assembles the uploaded parts into one object.
//
// The part list is rendered as the XML OSS expects and is covered by a
// Content-MD5, because a truncated or reordered list produces a silently
// corrupt object rather than an error. The Quark callback is replayed
// verbatim: it is what tells Quark the object is ready, so dropping it would
// leave the bytes in the bucket with no drive entry.
func (c *Client) commit(ctx context.Context, task UploadTask, etags []string) error {
	timeStr := time.Now().UTC().Format(http.TimeFormat)

	var xmlBuf bytes.Buffer
	xmlBuf.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n<CompleteMultipartUpload>\n")
	for i, etag := range etags {
		fmt.Fprintf(&xmlBuf, "<Part>\n<PartNumber>%d</PartNumber>\n<ETag>%s</ETag>\n</Part>\n", i+1, etag)
	}
	xmlBuf.WriteString("</CompleteMultipartUpload>")

	sum := md5.Sum(xmlBuf.Bytes())
	contentMD5 := base64.StdEncoding.EncodeToString(sum[:])
	callback := task.Callback
	if callback == "" {
		callback = "{}"
	}
	callbackB64 := base64.StdEncoding.EncodeToString([]byte(callback))

	canonical := fmt.Sprintf("POST\n%s\napplication/xml\n%s\nx-oss-callback:%s\nx-oss-date:%s\nx-oss-user-agent:%s\n/%s/%s?uploadId=%s",
		contentMD5, timeStr, callbackB64, timeStr, ossUserAgent,
		task.Bucket, task.ObjKey, task.UploadID)

	authKey, err := c.authFor(ctx, task, canonical)
	if err != nil {
		return err
	}

	u, err := url.Parse(task.ObjectURL(c.ossBase()))
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("uploadId", task.UploadID)
	u.RawQuery = q.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(xmlBuf.Bytes()))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", authKey)
	httpReq.Header.Set("Content-MD5", contentMD5)
	httpReq.Header.Set("Content-Type", "application/xml")
	httpReq.Header.Set("Referer", c.cfg.Referer)
	httpReq.Header.Set("x-oss-callback", callbackB64)
	httpReq.Header.Set("x-oss-date", timeStr)
	httpReq.Header.Set("x-oss-user-agent", ossUserAgent)

	res, err := c.http.Do(httpReq)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("%w: commit: OSS status %d: %s", ErrUploadRejected, res.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// finish tells Quark the object is assembled and should appear in the
// listing.
func (c *Client) finish(ctx context.Context, task UploadTask) error {
	body := map[string]any{
		"obj_key": task.ObjKey,
		"task_id": task.TaskID,
	}
	return c.do(ctx, EndpointUploadFinish, http.MethodPost, body, nil)
}

// ossUserAgent is a protocol constant: the OSS signature covers it, so it must
// be identical on the signing call and on the request.
const ossUserAgent = "aliyun-sdk-js/6.6.1 Chrome 98.0.4758.80 on Windows 10 64-bit"

// doNoRedirect issues one request that refuses to follow a redirect.
//
// Redirect following is decided by http.Client.CheckRedirect, which is a field
// on the client rather than the request. A per-request policy therefore needs
// its own client, built from the shared client's transport so connection
// pooling, timeouts and TLS settings are unchanged. That is safe because the
// shared client is never mutated: a new one is built per call.
func (c *Client) doNoRedirect(ctx context.Context, req *http.Request) (*http.Response, error) {
	client := &http.Client{
		Transport: c.http.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return client.Do(withRedirectRefusal(req))
}

// refuseRedirectHeader is the internal marker read by redirectRefuser.
const refuseRedirectHeader = "X-Quark-Refuse-Redirect"

// withRedirectRefusal marks a request so redirectRefuser strips the marker
// before the request goes on the wire.
//
// The flag rides on the request rather than on the client because only PRE has
// the no-replay property. Putting it on the shared client would also change
// the behaviour of every ordinary API call, and mutating a shared client per
// request is a data race.
func withRedirectRefusal(req *http.Request) *http.Request {
	cloned := req.Clone(req.Context())
	cloned.Header.Set(refuseRedirectHeader, "1")
	return cloned
}

// redirectRefuser wraps a transport so a request carrying the refusal marker
// is not sent with that marker on the wire.
//
// The real redirect refusal happens in the per-request client's
// CheckRedirect. This transport only exists to keep the internal marker out of
// the outgoing headers, because a header Quark does not expect can change how
// it answers.
type redirectRefuser struct{ base http.RoundTripper }

// RoundTrip implements http.RoundTripper.
func (r redirectRefuser) RoundTrip(req *http.Request) (*http.Response, error) {
	base := r.base
	if base == nil {
		base = http.DefaultTransport
	}
	if req.Header.Get(refuseRedirectHeader) == "" {
		return base.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	clone.Header.Del(refuseRedirectHeader)
	return base.RoundTrip(clone)
}

// buildUploadRequest builds an authenticated PRE request with an explicit
// cookie.
//
// It mirrors request() but takes the cookie from the caller, for the one
// request whose cookie must be captured as part of the call rather than read
// inside it. The header set is deliberately narrower than request(): it is
// exactly what the browser sends on PRE, and adding headers the provider does
// not expect can change how it answers.
func (c *Client) buildUploadRequest(ctx context.Context, ep Endpoint, cookie string, body any) (*http.Request, error) {
	u, err := url.JoinPath(c.cfg.BaseURL, string(ep))
	if err != nil {
		return nil, err
	}
	var payload []byte
	if body != nil {
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Referer", c.cfg.Referer)

	q := req.URL.Query()
	q.Set("pr", "ucpro")
	q.Set("fr", "pc")
	req.URL.RawQuery = q.Encode()
	return req, nil
}

// uploadRejection turns a provider envelope rejection into a categorised
// error.
//
// A name collision is the case that matters: Quark answers it with a message
// rather than a distinct code, so it is matched on the message text and mapped
// to a target conflict — the explicit no-overwrite contract. The rendered
// detail is static wording rather than the provider message, because the
// message is free-form text that travels into logs. Everything else follows
// the ordinary classification.
func (c *Client) uploadRejection(ep string, httpStatus, status, code int, message string) error {
	if isNameCollision(message) {
		return quarkerr.New(quarkerr.CatTargetConflict, ep, "a file with this name already exists at the destination")
	}
	return categorise(ep, &APIError{
		HTTPStatus: httpStatus,
		Status:     status,
		Code:       code,
		Message:    message,
	})
}

// isNameCollision recognises Quark's same-name rejection. The provider uses a
// bracketed Chinese tag rather than a distinct status code, and the wording is
// stable across the versions this was verified against.
func isNameCollision(message string) bool {
	m := strings.ToLower(message)
	return strings.Contains(m, "同名冲突") || strings.Contains(m, "same name")
}
