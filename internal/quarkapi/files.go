package quarkapi

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strconv"

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
func (c *Client) List(ctx context.Context, parentFID string) ([]*File, error) {
	var out []*File
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		q := sortRequest{
			PdirFID:           parentFID,
			Page:              page,
			FetchAllFile:      true,
			FetchRiskFileName: true,
		}
		var resp sortResponse
		if err := c.doQuery(ctx, EndpointFileSort, http.MethodGet, q.values(), nil, &resp); err != nil {
			return nil, err
		}
		for i := range resp.Data.List {
			out = append(out, resp.Data.List[i].toFile())
		}
		if resp.Metadata.Total > 0 {
			if page*PageSize >= resp.Metadata.Total {
				return out, nil
			}
		} else if len(resp.Data.List) < PageSize {
			return out, nil
		}
	}
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
