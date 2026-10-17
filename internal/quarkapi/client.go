package quarkapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/muou000/quark-rclone/internal/cookieutil"
	"github.com/muou000/quark-rclone/internal/quarkerr"
)

// The Quark web-client identity. The API rejects requests that do not carry a
// recognised User-Agent, so this string is a protocol constant rather than a
// preference.
const (
	DefaultBaseURL = "https://drive.quark.cn/1/clouddrive"
	DefaultReferer = "https://pan.quark.cn"
	DefaultUA      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) quark-cloud-drive/2.5.20 Chrome/100.0.4896.160 Electron/18.3.5.4-b478491100 Safari/537.36 Channel/pckk_other_ch"
)

// ErrNotAuthenticated is returned when candidate validation fails because
// Quark rejected the cookie. It carries the auth_required category so both
// errors.Is(err, ErrNotAuthenticated) and quarkerr.NeedsReauth(err) hold for
// the same failure. It is the only provider error that is safe to treat as
// "credentials are dead"; every other rejection may be transient or may
// concern a single FID.
var ErrNotAuthenticated = &quarkerr.Error{
	Category: quarkerr.CatAuthRequired,
	Detail:   "quark: authentication rejected",
}

// APIError is a rejected Quark response. The provider envelope is preserved so
// callers can match an exact signature instead of parsing message text.
type APIError struct {
	// HTTPStatus is the transport status.
	HTTPStatus int
	// Status and Code are the provider envelope fields.
	Status int
	Code   int
	// Message is the provider message, unchanged.
	Message string
}

// Error returns the provider message unchanged so callers may match on it.
func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("quark api error: http=%d status=%d code=%d", e.HTTPStatus, e.Status, e.Code)
}

// envelope is the response wrapper every Quark endpoint returns.
type envelope struct {
	Status  int    `json:"status"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Classify maps a rejected Quark response onto a stable error category.
//
// The provider's numeric codes are an implementation detail that can change, so
// the mapping keys on the shapes that are unlikely to move: the transport
// status plus the envelope's own status field. Anything unrecognised becomes
// CatUnknown rather than being guessed into a category that implies a remedy.
func Classify(apiErr *APIError) quarkerr.Category {
	if apiErr == nil {
		return quarkerr.CatUnknown
	}
	switch apiErr.HTTPStatus {
	case http.StatusUnauthorized:
		return quarkerr.CatAuthRequired
	case http.StatusForbidden:
		return quarkerr.CatPermissionDenied
	case http.StatusNotFound:
		return quarkerr.CatObjectNotFound
	case http.StatusConflict, http.StatusPreconditionFailed:
		// A real conditional-request failure may arrive as 412; ADR 0010 keeps
		// it in the same target-conflict category as 409.
		return quarkerr.CatTargetConflict
	case http.StatusLocked:
		return quarkerr.CatLocked
	case http.StatusTooManyRequests:
		return quarkerr.CatRateLimited
	case http.StatusInsufficientStorage:
		return quarkerr.CatQuotaExceeded
	case http.StatusRequestedRangeNotSatisfiable:
		return quarkerr.CatInvalidRequest
	}
	// The envelope can disagree with the transport, so it is consulted next.
	// A 200 transport carrying a rejected envelope is a provider-level failure.
	switch {
	case apiErr.Code == 31001 || apiErr.Code == 31002:
		// Session expired or invalidated (issue #2).
		return quarkerr.CatAuthRequired
	case apiErr.Status == http.StatusUnauthorized:
		// A 200 transport can still carry a rejected envelope whose own status
		// says the session is gone.
		return quarkerr.CatAuthRequired
	case apiErr.Status == http.StatusForbidden:
		return quarkerr.CatPermissionDenied
	case apiErr.Status == http.StatusNotFound:
		return quarkerr.CatObjectNotFound
	case apiErr.Status == http.StatusConflict || apiErr.Status == http.StatusPreconditionFailed:
		return quarkerr.CatTargetConflict
	case apiErr.Status == http.StatusLocked:
		return quarkerr.CatLocked
	case apiErr.Status == http.StatusTooManyRequests:
		return quarkerr.CatRateLimited
	case apiErr.Status >= 500:
		return quarkerr.CatUpstreamTransient
	case apiErr.Status >= 400:
		return quarkerr.CatInvalidRequest
	}
	return quarkerr.CatUnknown
}

// categorise wraps a provider rejection into the shared error vocabulary,
// preserving the original so a caller can still reach the raw status and code.
//
// The detail is the structured triple rather than the provider's message text:
// the message can echo request material, and Error() renders whatever is in
// Detail, so free-form provider text must not flow into it.
func categorise(op string, apiErr *APIError) error {
	wrapped := quarkerr.Wrap(Classify(apiErr), op, apiErr)
	wrapped.Detail = fmt.Sprintf("http=%d status=%d code=%d", apiErr.HTTPStatus, apiErr.Status, apiErr.Code)
	return wrapped
}

// nonEnvelopeError classifies a response whose body is not the provider
// envelope, so there is no status or code to read: a 4xx is the caller's
// fault and a 5xx is the provider's.
//
// The raw body rides on the cause for callers that need it, but the rendered
// detail stays structured, because a non-envelope body is exactly where an
// intermediary — a proxy, a captive portal — would echo request material back.
func nonEnvelopeError(op string, httpStatus int, raw []byte) error {
	cat := quarkerr.CatInvalidRequest
	if httpStatus >= 500 {
		cat = quarkerr.CatUpstreamTransient
	}
	wrapped := quarkerr.Wrap(cat, op, &APIError{
		HTTPStatus: httpStatus,
		Message:    strings.TrimSpace(string(raw)),
	})
	wrapped.Detail = fmt.Sprintf("http=%d: body is not the provider envelope", httpStatus)
	return wrapped
}

// Config holds the connection identity for a Quark account. Every field is an
// override for a protocol default; they exist so tests can point the client at
// a local server.
type Config struct {
	BaseURL       string
	Referer       string
	UserAgent     string
	UploadURLBase string
}

// Client talks to the Quark cloud-drive API.
//
// The client itself holds no credential state: every request reads a snapshot
// from the CredentialSource, and every response's session-refresh fields are
// merged back through it. This is what keeps the read-modify-write of a cookie
// rotation atomic against concurrent requests (ADR 0008).
type Client struct {
	cfg    Config
	http   *http.Client
	source CredentialSource
}

// New builds a Client. A nil httpClient uses a default with no timeout:
// streaming transfers are bounded by the request context, not a wall clock.
// It panics when source is nil: a client without a credential source is a
// wiring error, not a runtime condition.
func New(source CredentialSource, cfg Config, httpClient *http.Client) *Client {
	if source == nil {
		panic("quarkapi: nil CredentialSource")
	}
	base := cfg.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	ref := cfg.Referer
	if ref == "" {
		ref = DefaultReferer
	}
	ua := cfg.UserAgent
	if ua == "" {
		ua = DefaultUA
	}
	c := &Client{
		cfg: Config{
			BaseURL:       strings.TrimSuffix(base, "/"),
			Referer:       ref,
			UserAgent:     ua,
			UploadURLBase: cfg.UploadURLBase,
		},
		http:   httpClient,
		source: source,
	}
	if c.http == nil {
		c.http = &http.Client{}
	}
	// Wrap the transport so a PRE request can refuse to follow a redirect. The
	// wrapper only acts on requests carrying the marker, so every other call
	// is untouched.
	if rt := c.http.Transport; rt == nil || !isRefuser(rt) {
		c.http = &http.Client{
			Transport:     redirectRefuser{base: rt},
			CheckRedirect: c.http.CheckRedirect,
			Timeout:       c.http.Timeout,
			Jar:           c.http.Jar,
		}
	}
	return c
}

// isRefuser reports whether the transport is already wrapped, so a client that
// is re-wrapped does not nest another layer.
func isRefuser(rt http.RoundTripper) bool {
	_, ok := rt.(redirectRefuser)
	return ok
}

// Config returns the effective connection identity.
func (c *Client) Config() Config { return c.cfg }

// ValidateCandidate checks that a candidate cookie is accepted by Quark, using
// only the candidate: the active credential is neither read nor written.
//
// Both probes are side-effect free: /config only reads the session and the
// root listing only reads. A cookie is promoted to an active credential only
// after both succeed (ADR 0003), because a cookie that passes /config can
// still be scoped to an account whose drive listing is refused.
//
// Any session refresh fields observed in the probe responses are applied to
// the returned effective cookie, so the activated credential starts from the
// freshest session the provider handed out during validation.
func (c *Client) ValidateCandidate(ctx context.Context, cookie string) (string, error) {
	if strings.TrimSpace(cookie) == "" {
		return "", quarkerr.New(quarkerr.CatAuthRequired, "verify", "cookie is empty")
	}
	effective := cookie
	fields, err := c.doExplicit(ctx, EndpointConfig, http.MethodGet, nil, nil, effective, nil)
	if err != nil {
		return "", classifyAuth(err)
	}
	effective = applyRefresh(effective, fields)

	// The root FID is a protocol constant, so this listing needs no prior
	// call. One page is sufficient evidence: the same permission governs
	// every page, and validation must stay cheap.
	fields, err = c.doExplicit(ctx, EndpointFileSort, http.MethodGet, firstPageQuery(RootFID), nil, effective, &sortResponse{})
	if err != nil {
		return "", classifyAuth(err)
	}
	return applyRefresh(effective, fields), nil
}

// RefreshSessionCookie asks the provider to reissue the session cookie.
//
// The server only reissues __puus when the request omits it (AlistGo/alist#830
// as confirmed by issue #2), so the probe is sent with the field stripped from
// a snapshot. The stripped value is never written back: refresh fields from
// the response are merged on top of the credential's CURRENT value, so a
// concurrent rotation cannot be clobbered and a failed refresh leaves the
// stored cookie untouched.
func (c *Client) RefreshSessionCookie(ctx context.Context) error {
	stripped := cookieutil.Del(c.source.CookieSnapshot(), "__puus")
	fields, err := c.doExplicit(ctx, EndpointConfig, http.MethodGet, nil, nil, stripped, nil)
	if err != nil {
		return err
	}
	// A probe that succeeded without a reissue still proves the session is
	// alive; there is simply nothing to merge.
	if len(fields) > 0 {
		c.source.MergeRefresh(fields)
	}
	return nil
}

// ProbeConfig performs the cheapest authenticated call — /config — with the
// current credential and no side effects beyond the response's refresh
// merge. It is the real-readiness probe ADR 0004 asks a supervisor to run
// before rclone starts.
func (c *Client) ProbeConfig(ctx context.Context) error {
	_, err := c.doExplicit(ctx, EndpointConfig, http.MethodGet, nil, nil, c.source.CookieSnapshot(), nil)
	return err
}

// applyRefresh merges observed refresh fields into a cookie string.
func applyRefresh(cookie string, fields map[string]string) string {
	for name, value := range fields {
		cookie = cookieutil.Set(cookie, name, value)
	}
	return cookie
}

// classifyAuth promotes an authentication-shaped failure to
// ErrNotAuthenticated so callers can detect a dead credential without knowing
// the provider's codes. The categorised cause is wrapped alongside, so the
// category survives the sentinel. Every other failure is returned unchanged.
func classifyAuth(err error) error {
	if quarkerr.IsCategory(err, quarkerr.CatAuthRequired) {
		return fmt.Errorf("%w: %w", ErrNotAuthenticated, err)
	}
	return err
}

// firstPageQuery renders the /file/sort query for the first page of a
// directory. It exists so validation can probe listing without paging.
func firstPageQuery(parentFID string) map[string]string {
	return (sortRequest{
		PdirFID:           parentFID,
		Page:              1,
		FetchAllFile:      true,
		FetchRiskFileName: true,
	}).values()
}

// request builds one authenticated API request with an explicit cookie. Query
// parameters are applied after the fixed pr/fr pair so a caller value cannot
// silently drop them.
func (c *Client) request(ctx context.Context, ep Endpoint, method string, query map[string]string, body any, cookie string) (*http.Request, error) {
	u, err := url.JoinPath(c.cfg.BaseURL, string(ep))
	if err != nil {
		return nil, err
	}
	var payload []byte
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Referer", c.cfg.Referer)
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Origin", c.cfg.Referer)

	q := req.URL.Query()
	q.Set("pr", "ucpro")
	q.Set("fr", "pc")
	for k, v := range query {
		q.Set(k, v)
	}
	req.URL.RawQuery = q.Encode()
	return req, nil
}

// extractRefresh returns the session refresh fields observed in a response.
func extractRefresh(res *http.Response) map[string]string {
	var fields map[string]string
	for _, ck := range res.Cookies() {
		if refreshField(ck.Name) {
			if fields == nil {
				fields = make(map[string]string)
			}
			fields[ck.Name] = ck.Value
		}
	}
	return fields
}

// decodeEnvelope reads a response, decodes the provider envelope into result
// when it is non-nil, and returns the refresh fields observed on the wire. It
// does not touch the credential source: merging is the caller's decision.
func (c *Client) decodeEnvelope(res *http.Response, ep Endpoint, result any) (map[string]string, error) {
	defer res.Body.Close()
	fields := extractRefresh(res)

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return fields, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		if res.StatusCode >= 400 {
			return fields, nonEnvelopeError(string(ep), res.StatusCode, raw)
		}
		return fields, fmt.Errorf("quark: decode %s: %w", ep, err)
	}
	if env.Status >= 400 || env.Code != 0 || res.StatusCode >= 400 {
		apiErr := &APIError{
			HTTPStatus: res.StatusCode,
			Status:     env.Status,
			Code:       env.Code,
			Message:    env.Message,
		}
		return fields, categorise(string(ep), apiErr)
	}
	if result != nil {
		if err := json.Unmarshal(raw, result); err != nil {
			return fields, fmt.Errorf("quark: decode %s data: %w", ep, err)
		}
	}
	return fields, nil
}

// do performs one API call with the current credential snapshot, merging any
// observed refresh fields back into the source.
func (c *Client) do(ctx context.Context, ep Endpoint, method string, body any, result any) error {
	return c.doQuery(ctx, ep, method, nil, body, result)
}

// doQuery is do with query parameters applied.
func (c *Client) doQuery(ctx context.Context, ep Endpoint, method string, query map[string]string, body any, result any) error {
	req, err := c.request(ctx, ep, method, query, body, c.source.CookieSnapshot())
	if err != nil {
		return err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	fields, derr := c.decodeEnvelope(res, ep, result)
	if len(fields) > 0 {
		c.source.MergeRefresh(fields)
	}
	return derr
}

// doExplicit performs one API call with an explicit cookie and returns the
// observed refresh fields instead of merging them. It is the path for requests
// that must not touch the active credential: candidate validation and the
// stripped-cookie session refresh.
func (c *Client) doExplicit(ctx context.Context, ep Endpoint, method string, query map[string]string, body any, cookie string, result any) (map[string]string, error) {
	req, err := c.request(ctx, ep, method, query, body, cookie)
	if err != nil {
		return nil, err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	return c.decodeEnvelope(res, ep, result)
}
