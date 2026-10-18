package quarkapi

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/muou000/quark-rclone/internal/quarkerr"
)

// PageSize is the directory page size Quark accepts for /file/sort.
const PageSize = 100

// ErrNoDownloadURL is returned when Quark answers /file/download without a
// URL. It means the FID is not currently resolvable, which is distinct from a
// transport failure.
var ErrNoDownloadURL = errors.New("quark: no download url in response")

// sortRequest is the /file/sort query.
type sortRequest struct {
	PdirFID           string
	Page              int
	Sort              string
	OrderDirection    string
	FetchAllFile      bool
	FetchRiskFileName bool
}

// sortResponse is the /file/sort payload.
type sortResponse struct {
	envelope
	Data struct {
		List []rawFile `json:"list"`
	} `json:"data"`
	Metadata struct {
		Total int `json:"_total"`
	} `json:"metadata"`
}

// rawFile is one listing entry. Only the fields the mount client needs are
// bound; the rest of the provider payload is ignored so a field change cannot
// break decoding.
type rawFile struct {
	Fid       string `json:"fid"`
	FileName  string `json:"file_name"`
	Category  int    `json:"category"`
	Size      int64  `json:"size"`
	File      bool   `json:"file"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// toFile converts a raw entry into a domain File.
func (r *rawFile) toFile() *File {
	kind := KindDir
	if r.File {
		kind = KindFile
	}
	return &File{
		FID:       r.Fid,
		Name:      html.UnescapeString(r.FileName),
		Kind:      kind,
		Size:      r.Size,
		Category:  r.Category,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
}

// List returns every entry in a directory, following pages until the provider
// reports the total is exhausted. The FID is the only stable object identity:
// names can change between pages and are never used to address an object.
//
// The first page lands serially because it is the one that reports the total,
// and a single-page directory — the common case — must not pay for fan-out.
// Every remaining page is then fetched by a bounded worker pool, so a folder
// that spans N pages costs about one round-trip instead of N. The merge stays
// in page order, so the listing is deterministic regardless of which worker
// finishes first.
func (c *Client) List(ctx context.Context, parentFID string) ([]*File, error) {
	first, total, err := c.listPage(ctx, parentFID, 1)
	if err != nil {
		return nil, err
	}
	// Preallocate from the reported total, but bounded: the provider's total is
	// a hint that can over-report (and a broken one can be absurd), and a
	// preallocation must never become a memory-amplification vector of its own.
	out := make([]*File, 0, listingCap(total))
	out = append(out, first...)

	if total <= 0 {
		// The provider reported no total. Keep the serial contract: fetch
		// until a short page. Guessing a page count from the first page
		// alone would either truncate the listing or over-fetch it.
		pageLen := len(first)
		for page := 2; pageLen == PageSize; page++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			files, _, err := c.listPage(ctx, parentFID, page)
			if err != nil {
				return nil, err
			}
			out = append(out, files...)
			pageLen = len(files)
		}
		return out, nil
	}

	pages := (total + PageSize - 1) / PageSize
	rest, err := c.listPages(ctx, parentFID, 2, pages)
	if err != nil {
		return nil, err
	}
	for _, r := range rest {
		out = append(out, r.files...)
	}

	lastPage, lastLen := 1, len(first)
	if n := len(rest); n > 0 {
		lastPage, lastLen = rest[n-1].page, len(rest[n-1].files)
	}
	// A provider whose `_total` under-reports would truncate the listing here,
	// and a truncated listing is indistinguishable from a folder that lost
	// files. So the reported total is a hint, not a promise: while the last page
	// of the fan-out came back full, more may exist, and the listing is extended
	// one page at a time until one is short or empty, which proves the end. The
	// empty-page rule matters just as much as the short one: a total that wildly
	// over-reports would otherwise have the client ask for every page up to the
	// number it made up.
	for lastLen == PageSize {
		lastPage++
		files, _, err := c.listPage(ctx, parentFID, lastPage)
		if err != nil {
			return nil, err
		}
		out = append(out, files...)
		lastLen = len(files)
	}
	return dedupeByFID(out), nil
}

// dedupeByFID folds repeated FIDs down to their first occurrence.
//
// The FID is the one stable identity the provider guarantees; a name is
// display text that can repeat. Concurrent page fetches assume the provider's
// ordering holds still between pages, and when it does not the same entry can
// be served on two pages while another is skipped. A duplicate is harmless
// here but poisonous downstream: the WebDAV layer keys a directory by name, so
// a repeated entry silently overwrites its twin and the listing loses one of
// them. Folding on the FID keeps the listing exactly as wide as the entries
// actually observed.
//
// It returns a new slice rather than compacting in place: the input is the same
// backing array the caller may still be holding (a page result, or the zero-total
// loop's accumulator), and writing into it under the caller is a surprising side
// effect for a function that reads like a query.
func dedupeByFID(files []*File) []*File {
	seen := make(map[string]struct{}, len(files))
	out := make([]*File, 0, len(files))
	for _, f := range files {
		if _, dup := seen[f.FID]; dup {
			continue
		}
		seen[f.FID] = struct{}{}
		out = append(out, f)
	}
	return out
}

// listConcurrency caps how many listing pages are in flight at once. Quark is
// a reversed private API, so a polite ceiling beats maximum parallelism; eight
// concurrent pages already turns a hundred-page folder's wall clock into a
// handful of round-trips, and it bounds the pressure a single click puts on
// the account.
const listConcurrency = 8

// listPreallocCap bounds the listing slice's initial capacity. The provider's
// total is a hint that can over-report, and a preallocation sized by an
// untrusted number turns a bogus total into a memory spike before a single
// entry has been read. Growth past the cap is ordinary append amortisation.
const listPreallocCap = 4096

// listingCap is the initial capacity to allocate for a listing of a claimed
// size. The claim comes straight from the provider, so it is clamped to zero at
// the bottom and to listPreallocCap at the top before it is allowed to size an
// allocation.
func listingCap(claimed int) int {
	if claimed < 0 {
		return 0
	}
	if claimed > listPreallocCap {
		return listPreallocCap
	}
	return claimed
}

// pageResult is one fetched listing page, tagged with the page number that
// produced it. The tag is what lets the caller reassemble page order from a
// fan-out that completes in an arbitrary order.
type pageResult struct {
	page  int
	files []*File
}

// listPages fetches pages first..last (inclusive) with a bounded worker pool
// and returns them in page order.
//
// The page count the caller derived from `_total` is an upper bound, not a job
// list: a response with an absurd total would otherwise have the pool try to
// fetch a million pages and preallocate the results to match. So the feeder
// publishes page numbers lazily, and a worker that finds a short or empty page
// marks the listing as ended past that page — a provider that returns a partial
// page has no entries beyond it, whatever the total claimed.
//
// A short page stops the feeder. It does NOT cancel the pool: the workers still
// in flight hold results for pages below the end, and cancelling on a short page
// would drop data that was already fetched. Only a genuine page failure cancels,
// and even then the error is what the caller sees, so no partial listing can
// emerge from a failed fetch.
func (c *Client) listPages(ctx context.Context, parentFID string, first, last int) ([]pageResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// lastPage is the highest page the listing can still contain. Lowered by the
	// first worker that sees a short page: entries beyond it cannot exist.
	var lastPage atomic.Int64
	lastPage.Store(int64(last))

	work := make(chan int)
	done := make(chan pageResult, listConcurrency)
	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	fail := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}
	for range min(listConcurrency, max(last-first+1, 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for page := range work {
				files, _, err := c.listPage(ctx, parentFID, page)
				if err != nil {
					fail(err)
					return
				}
				if len(files) < PageSize {
					// A short or empty page is the end of the listing, so no
					// page beyond it may be published.
					for {
						cur := lastPage.Load()
						if int64(page) >= cur || lastPage.CompareAndSwap(cur, int64(page)) {
							break
						}
					}
				}
				select {
				case done <- pageResult{page: page, files: files}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(work)
		for page := first; page <= last; page++ {
			// Stop publishing past the page where the listing ended. The
			// pages already in flight are allowed to finish: their results are
			// data, and dropping them loses entries.
			if int64(page) > lastPage.Load() {
				return
			}
			select {
			case work <- page:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(done)
	}()

	results := make([]pageResult, 0, max(last-first+1, 0))
	for r := range done {
		results = append(results, r)
	}
	if firstErr != nil {
		return nil, firstErr
	}
	sort.Slice(results, func(i, j int) bool { return results[i].page < results[j].page })
	return results, nil
}

// listPage fetches one listing page and reports the provider's total.
func (c *Client) listPage(ctx context.Context, parentFID string, page int) ([]*File, int, error) {
	q := sortRequest{
		PdirFID:           parentFID,
		Page:              page,
		FetchAllFile:      true,
		FetchRiskFileName: true,
	}
	var resp sortResponse
	if err := c.doQuery(ctx, EndpointFileSort, http.MethodGet, q.values(), nil, &resp); err != nil {
		return nil, 0, err
	}
	files := make([]*File, 0, len(resp.Data.List))
	for i := range resp.Data.List {
		files = append(files, resp.Data.List[i].toFile())
	}
	return files, resp.Metadata.Total, nil
}

// values renders the request as query parameters.
func (r sortRequest) values() map[string]string {
	return map[string]string{
		"pdir_fid":             r.PdirFID,
		"_page":                strconv.Itoa(r.Page),
		"_size":                strconv.Itoa(PageSize),
		"_fetch_total":         "1",
		"_sort":                r.Sort,
		"fetch_all_file":       boolStr(r.FetchAllFile),
		"fetch_risk_file_name": boolStr(r.FetchRiskFileName),
	}
}

// boolStr renders a bool the way Quark's query parser expects.
func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// downloadResponse is the /file/download payload.
type downloadResponse struct {
	envelope
	Data []struct {
		DownloadURL string `json:"download_url"`
	} `json:"data"`
}

// DownloadURL mints a CDN URL for one FID and returns it together with the
// headers the URL is signed against.
//
// The snapshot rule is the whole point of this method (issue #2): Quark derives
// the URL signature from the cookie presented on this request, so the Cookie,
// Referer and User-Agent captured here must be replayed verbatim when fetching
// the URL. Fetching it without them, or with a rotated cookie, is a 403. The
// minted URL is therefore always paired with the exact snapshot it was signed
// against, never handed out bare.
func (c *Client) DownloadURL(ctx context.Context, fid string) (*DownloadResult, error) {
	// Snapshot before minting: the response may reissue __puus, and the minted
	// URL would then be signed against a cookie the caller no longer holds.
	cookie := c.source.CookieSnapshot()
	body := map[string]any{"fids": []string{fid}}
	req, err := c.request(ctx, EndpointFileDownload, http.MethodPost, nil, body, cookie)
	if err != nil {
		return nil, err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	var resp downloadResponse
	fields, derr := c.decodeEnvelope(res, EndpointFileDownload, &resp)
	if len(fields) > 0 {
		c.source.MergeRefresh(fields)
	}
	if derr != nil {
		return nil, derr
	}
	if len(resp.Data) == 0 || resp.Data[0].DownloadURL == "" {
		return nil, ErrNoDownloadURL
	}
	return &DownloadResult{
		URL: resp.Data[0].DownloadURL,
		Header: Header{
			"Cookie":     cookie,
			"Referer":    c.cfg.Referer,
			"User-Agent": c.cfg.UserAgent,
		},
	}, nil
}

// mkdirRequest is the POST /file body.
type mkdirRequest struct {
	DirInitLock bool   `json:"dir_init_lock"`
	DirPath     string `json:"dir_path"`
	FileName    string `json:"file_name"`
	PdirFID     string `json:"pdir_fid"`
}

// mkdirResponse is the POST /file payload.
type mkdirResponse struct {
	envelope
	Data struct {
		FID string `json:"fid"`
	} `json:"data"`
}

// Mkdir creates a directory and returns its FID.
//
// Quark answers success for a same-name directory that already exists, so a
// returned FID cannot be assumed to be new. When the response carries no FID
// the parent is listed to resolve it; a directory that is already present is
// accepted rather than reported as an error.
func (c *Client) Mkdir(ctx context.Context, parentFID, name string) (string, error) {
	body := mkdirRequest{
		DirInitLock: false,
		DirPath:     "",
		FileName:    name,
		PdirFID:     parentFID,
	}
	var resp mkdirResponse
	if err := c.do(ctx, EndpointFileCreate, http.MethodPost, body, &resp); err != nil {
		return "", err
	}
	if resp.Data.FID != "" {
		return resp.Data.FID, nil
	}
	return c.findChildByName(ctx, parentFID, name, true)
}

// findChildByName scans a directory for a child with an exact name.
//
// A miss is classified as object_not_found rather than left uncategorised: the
// caller is a move destination check or a Mkdir follow-up, and both need to
// tell "absent" apart from "the provider is broken".
func (c *Client) findChildByName(ctx context.Context, parentFID, name string, dirOnly bool) (string, error) {
	entries, err := c.List(ctx, parentFID)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Name == name && (!dirOnly || e.IsDir()) {
			return e.FID, nil
		}
	}
	return "", quarkerr.New(quarkerr.CatObjectNotFound, "findChild",
		fmt.Sprintf("directory %q not present in parent %s", name, parentFID))
}

// Rename renames a file or directory in place. The FID is unaffected: it is
// the stable identity the caller keeps using afterwards.
func (c *Client) Rename(ctx context.Context, fid, newName string) error {
	body := map[string]any{"fid": fid, "file_name": newName}
	return c.do(ctx, EndpointFileRename, http.MethodPost, body, nil)
}

// MoveRequest describes a move into a new parent.
type MoveRequest struct {
	FileIDs  []string
	ToPdirID string
}

// Move relocates files or directories.
func (c *Client) Move(ctx context.Context, req MoveRequest) error {
	body := map[string]any{
		"action_type":  1,
		"exclude_fids": []string{},
		"filelist":     req.FileIDs,
		"to_pdir_fid":  req.ToPdirID,
	}
	return c.do(ctx, EndpointFileMove, http.MethodPost, body, nil)
}

// DeleteRequest describes a delete to the recycle bin.
type DeleteRequest struct {
	FileIDs []string
}

// Delete moves files or directories to the recycle bin.
//
// This is the raw control operation: it is issued exactly once with no
// automatic replay, because a lost response may still have performed the
// delete. Callers that need uncertainty handled must use DeleteReliable.
func (c *Client) Delete(ctx context.Context, req DeleteRequest) error {
	body := map[string]any{
		"action_type":  1,
		"exclude_fids": []string{},
		"filelist":     req.FileIDs,
	}
	return c.do(ctx, EndpointFileDelete, http.MethodPost, body, nil)
}

// fileInfoResponse is the /file/info payload. Only the FID is bound: presence
// is decided by exact FID equality and by nothing else the provider returns.
type fileInfoResponse struct {
	envelope
	Data *struct {
		Fid string `json:"fid"`
	} `json:"data"`
}

// quarkFileNotFoundCode is the provider code Quark returns from /file/info
// for a FID that no longer exists. Observed against the real provider as HTTP
// 404 with status 404 and this code; it is the only signature that may be
// read as absence (issue #2).
const quarkFileNotFoundCode = 21001

// FIDExists resolves a single FID through /file/info.
//
// Presence requires HTTP 200, a successful envelope and a data object whose
// fid equals the requested one. Absence requires the exact not-found signature
// — HTTP 404, provider status 404 and code 21001 all together. Everything
// else (an invalid-FID rejection, an auth failure, a rate limit, a gateway
// page, malformed JSON, a missing data object, or a mismatched fid) is an
// error, never absence: a destructive caller must not act on a guess.
func (c *Client) FIDExists(ctx context.Context, fid string) (bool, error) {
	if fid == "" {
		return false, quarkerr.New(quarkerr.CatInvalidRequest, "file_info", "fid is required")
	}
	var resp fileInfoResponse
	err := c.doQuery(ctx, EndpointFileInfo, http.MethodGet, map[string]string{"fid": fid}, nil, &resp)
	if err != nil {
		if isFileNotFoundSignature(err) {
			return false, nil
		}
		return false, err
	}
	if resp.Data == nil {
		return false, quarkerr.New(quarkerr.CatUnknown, "file_info",
			fmt.Sprintf("provider returned no data for fid %s", fid))
	}
	if resp.Data.Fid != fid {
		return false, quarkerr.New(quarkerr.CatUnknown, "file_info",
			fmt.Sprintf("provider returned fid %s for requested fid %s", resp.Data.Fid, fid))
	}
	return true, nil
}

// isFileNotFoundSignature reports whether err is the exact provider signature
// for a FID that no longer exists. All three fields must match: a 404 without
// the provider envelope, or the envelope without the HTTP status, is not proof
// of absence. Matching is structural; the message text is never inspected.
func isFileNotFoundSignature(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.HTTPStatus == http.StatusNotFound &&
		apiErr.Status == http.StatusNotFound &&
		apiErr.Code == quarkFileNotFoundCode
}
