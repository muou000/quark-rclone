// Package adapter is the public service layer of the Quark mount client.
//
// Everything below this package is a mechanism with a narrow job; this package
// is the one a mount, a CLI or a future rclone backend talks to. It exposes
// the operations the acceptance criteria name — list, download, upload,
// mkdir, move, rename, delete — plus the capability statement callers need to
// decide what to even attempt.
package adapter

import (
	"context"
	"io"

	"github.com/muou000/quark-rclone/internal/quarkapi"
	"github.com/muou000/quark-rclone/internal/quarkerr"
	"github.com/muou000/quark-rclone/internal/session"
)

// Capabilities is the adapter's honest statement of what the Quark backend can
// and cannot do. A caller consults this instead of probing by trial, because a
// failed probe costs a request against the provider and an error the caller
// must then misclassify.
type Capabilities struct {
	// Copy is always false: Quark's web API exposes no copy endpoint for the
	// drive, and the issue-2 contract is explicit that Copy is unsupported
	// rather than emulated. An emulation via download+upload would silently
	// mint a new FID, breaking the FID-as-identity assumption every caller
	// makes.
	Copy bool
	// OverwriteUpload is always false: Quark refuses a same-name upload, which
	// the adapter surfaces as CatTargetConflict rather than replacing the
	// existing object.
	OverwriteUpload bool
}

// Service is the Quark adapter's public operations.
type Service struct {
	api   *quarkapi.Client
	store *session.Store
	caps  Capabilities
}

// New wires a Service from a live wire client and the credential store.
func New(api *quarkapi.Client, store *session.Store) *Service {
	return &Service{
		api:   api,
		store: store,
		caps:  CapabilitiesOf(),
	}
}

// CapabilitiesOf states what the Quark backend supports. It is a function
// rather than a variable so callers cannot mutate the statement another
// caller read.
//
// Both fields are false and stay false; they exist so the capability
// statement is data a caller can read rather than documentation a caller must
// remember.
func CapabilitiesOf() Capabilities {
	return Capabilities{Copy: false, OverwriteUpload: false}
}

// Capabilities reports what the backend supports.
func (s *Service) Capabilities() Capabilities { return s.caps }

// CookieSnapshot exposes the current credential, redacted by the caller's
// logging policy. It exists for diagnostics endpoints that show session state
// without ever printing the value.
func (s *Service) CookieSnapshot() string { return s.store.CookieSnapshot() }

// ValidateAndActivate promotes a candidate cookie to the active credential
// after both validation probes pass. On failure the current credential is
// preserved.
func (s *Service) ValidateAndActivate(ctx context.Context, cookie string) error {
	return s.store.ValidateAndActivate(ctx, s.api, cookie)
}

// List returns every entry of a directory by FID.
func (s *Service) List(ctx context.Context, parentFID string) ([]*quarkapi.File, error) {
	return s.api.List(ctx, parentFID)
}

// DownloadURL mints a CDN URL for a FID together with the headers it is signed
// against. Callers must replay those headers verbatim when fetching.
func (s *Service) DownloadURL(ctx context.Context, fid string) (*quarkapi.DownloadResult, error) {
	return s.api.DownloadURL(ctx, fid)
}

// UploadRequest describes a file upload.
type UploadRequest = quarkapi.UploadRequest

// Upload streams a file into a directory and returns the assigned FID. A
// same-name collision is CatTargetConflict: the existing object is never
// replaced.
func (s *Service) Upload(ctx context.Context, req UploadRequest, body io.Reader, onProgress func(sent int64)) (string, error) {
	return s.api.Upload(ctx, req, body, onProgress)
}

// Mkdir creates a directory and returns its FID.
func (s *Service) Mkdir(ctx context.Context, parentFID, name string) (string, error) {
	return s.api.Mkdir(ctx, parentFID, name)
}

// Move relocates objects into a new parent FID.
func (s *Service) Move(ctx context.Context, fids []string, toParentFID string) error {
	return s.api.Move(ctx, quarkapi.MoveRequest{FileIDs: fids, ToPdirID: toParentFID})
}

// Rename changes an object's name. The FID is unaffected.
func (s *Service) Rename(ctx context.Context, fid, newName string) error {
	return s.api.Rename(ctx, fid, newName)
}

// Delete removes one object, handling the ambiguous outcomes a raw delete
// cannot distinguish on its own. See quarkapi.DeleteReliable.
func (s *Service) Delete(ctx context.Context, fid string) error {
	return s.api.DeleteReliable(ctx, fid)
}

// Copy is the explicit refusal. Quark has no copy endpoint, and emulating one
// by download+upload would mint a new FID and break the identity assumption.
// The operation reports CatNotSupported so a caller answers 405 rather than
// attempting a fallback.
func (s *Service) Copy(ctx context.Context, fid, toParentFID, newName string) error {
	return quarkerr.NotSupported("copy")
}

// RefreshSessionCookie asks the provider to reissue the session cookie now,
// outside the periodic loop.
func (s *Service) RefreshSessionCookie(ctx context.Context) error {
	return s.api.RefreshSessionCookie(ctx)
}
