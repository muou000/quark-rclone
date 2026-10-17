// Package quarkapi implements the Quark (夸克网盘) private web API layer used
// by the Quark desktop web client. It is deliberately narrow: only the
// endpoints the mount client needs are implemented, so the surface stays small
// and the request shape stays faithful to what the browser sends.
//
// The package is the wire layer of the adapter (ADR 0008): it knows endpoints,
// envelopes, headers and the session-refresh field names, and nothing about
// credentials lifecycle, mounts or persistence.
package quarkapi

// RootFID is the FID of the Quark drive root. It is a protocol constant, so a
// caller can list the root without first resolving a path.
const RootFID = "0"

// Endpoint is a single Quark cloud-drive API route.
type Endpoint string

const (
	// EndpointConfig returns the account bootstrap payload. It is the cheapest
	// authenticated call, so it doubles as a cookie liveness probe.
	EndpointConfig Endpoint = "/config"
	// EndpointFileSort lists one directory page.
	EndpointFileSort Endpoint = "/file/sort"
	// EndpointFileDownload mints a time-limited CDN download URL for a FID.
	EndpointFileDownload Endpoint = "/file/download"
	// EndpointFileCreate creates a directory.
	EndpointFileCreate Endpoint = "/file"
	// EndpointFileInfo resolves a single FID's existence.
	EndpointFileInfo Endpoint = "/file/info"
	// EndpointFileRename renames a file or directory.
	EndpointFileRename Endpoint = "/file/rename"
	// EndpointFileMove moves files or directories into another parent.
	EndpointFileMove Endpoint = "/file/move"
	// EndpointFileDelete moves files or directories to the recycle bin.
	EndpointFileDelete Endpoint = "/file/delete"
	// EndpointUploadPre allocates an upload task. It is the only request in
	// the whole flow that must never be retried: a lost response may still
	// have allocated a task and a FID on the provider side.
	EndpointUploadPre Endpoint = "/file/upload/pre"
	// EndpointUploadHash offers whole-file hashes so Quark can deduplicate an
	// identical upload instead of transferring the bytes.
	EndpointUploadHash Endpoint = "/file/update/hash"
	// EndpointUploadAuth mints a short-lived signature for one OSS operation.
	// It signs the part upload and the commit separately, so it is called once
	// per operation rather than once per file.
	EndpointUploadAuth Endpoint = "/file/upload/auth"
	// EndpointUploadFinish tells Quark the object is assembled and should
	// become visible in the drive listing.
	EndpointUploadFinish Endpoint = "/file/upload/finish"
)

// DefaultUploadPartSize is the upload chunk size used when the provider does
// not report one. Quark's own web client sends 4 MiB parts; using the provider
// value when present keeps this client byte-compatible with the browser.
const DefaultUploadPartSize = 4 << 20

// CredentialSource supplies the cookie snapshot for each request and absorbs
// the session refresh fields observed in responses.
//
// The contract (ADR 0008): every request uses a snapshot of the current
// cookie; refresh fields are merged on top of the CURRENT credential — never
// the value the request was sent with — so a late response cannot clobber a
// newer concurrent update, and the merge notifies the supervisor to persist.
type CredentialSource interface {
	// CookieSnapshot returns the current cookie value for one request.
	CookieSnapshot() string
	// MergeRefresh merges observed refresh fields (e.g. __puus, __pus) into
	// the current credential and triggers the persist notification.
	MergeRefresh(fields map[string]string)
}

// refreshField reports whether a response cookie name is a session refresh
// field. __puus carries the session; __pus additionally rotates when the
// transcode feature is active (issue #2).
func refreshField(name string) bool {
	switch name {
	case "__puus", "__pus":
		return true
	}
	return false
}

// FileKind reports whether an entry is a directory or a file.
type FileKind bool

const (
	// KindFile marks a regular file.
	KindFile FileKind = true
	// KindDir marks a directory.
	KindDir FileKind = false
)

// File is one entry in a Quark directory listing.
type File struct {
	// FID is the provider-assigned immutable identifier. It is the only value
	// used to address the file; names are never used for identity.
	FID string
	// Name is the display name as returned by the provider.
	Name string
	// Kind distinguishes files from directories.
	Kind FileKind
	// Size is the file size in bytes. It is zero for directories.
	Size int64
	// Category is the provider content category. Videos are category 1.
	Category int
	// CreatedAt and UpdatedAt are millisecond Unix timestamps.
	CreatedAt int64
	UpdatedAt int64
}

// IsDir reports whether the entry is a directory.
func (f *File) IsDir() bool { return f.Kind == KindDir }

// DownloadResult is one minted CDN URL plus the request headers Quark expects
// the client to replay verbatim.
type DownloadResult struct {
	// URL is the time-limited CDN URL. It must not be handed to a browser:
	// Quark binds it to the Cookie and Referer used to mint it.
	URL string
	// Header carries the Cookie, Referer and User-Agent that the URL is signed
	// against. A caller that fetches the URL without replaying them gets 403.
	Header Header
}

// Header is a case-insensitive set of HTTP headers.
type Header map[string]string

// Clone returns a deep copy so callers cannot mutate a shared result.
func (h Header) Clone() Header {
	if h == nil {
		return nil
	}
	out := make(Header, len(h))
	for k, v := range h {
		out[k] = v
	}
	return out
}

// UploadTask is the provider-side allocation returned by /file/upload/pre.
//
// The FID is allocated before any byte is sent, which is what makes the task
// resumable and what makes a lost PRE response ambiguous: the task may exist
// even though the caller never saw it. Upload therefore treats PRE as a single
// attempt.
type UploadTask struct {
	// TaskID identifies the upload session across the auth, part and finish
	// calls.
	TaskID string
	// UploadID is the OSS multipart upload identifier.
	UploadID string
	// ObjKey is the object key inside the OSS bucket.
	ObjKey string
	// Bucket and UploadURLHost locate the OSS endpoint. UploadURLHost is the
	// host suffix only, so a caller builds "https://<bucket>.<host>/<objKey>".
	Bucket        string
	UploadURLHost string
	// FID is pre-allocated by Quark. It is the only value that can confirm the
	// object landed, because a name and size can be shared by an older object.
	FID string
	// AuthInfo is an opaque token Quark exchanges for a per-operation OSS
	// signature. It is passed back on every auth call.
	AuthInfo string
	// Callback is the JSON Quark sends to its own callback URL once the OSS
	// commit succeeds. It must be replayed verbatim on the commit request.
	Callback string
	// PartSize is the chunk size the provider wants. Zero means the caller
	// should use DefaultUploadPartSize.
	PartSize int64
}

// ObjectURL is the OSS address the parts are written to.
//
// A test cannot resolve a synthetic bucket host, so it supplies one through
// Config.UploadURLBase, which replaces the provider host while keeping the
// object key intact. That keeps the URL shape under test identical to the real
// one instead of routing the request somewhere structurally different.
func (t UploadTask) ObjectURL(base string) string {
	if base != "" {
		return base + "/" + t.ObjKey
	}
	return "https://" + t.Bucket + "." + t.UploadURLHost + "/" + t.ObjKey
}
