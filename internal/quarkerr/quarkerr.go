// Package quarkerr defines the stable error categories that every layer of the
// mount client shares.
//
// The WebDAV endpoint, the rclone/VFS layer, a mount supervisor state machine
// and the tray UI all need to agree on what a failure means. Without a shared
// vocabulary each layer invents its own, and a provider rejection ends up
// reported as a generic EIO or "network error" that tells the user nothing
// about what to do next.
//
// A category is decided once, from the provider's own response (see
// quarkapi.Classify), and then mapped outward. The mapping to an HTTP status is
// a presentation concern and lives here too so the two cannot drift apart.
package quarkerr

import (
	"errors"
	"fmt"
	"net/http"
	"syscall"
)

// Category is a stable failure class. It is what the rest of the program
// switches on; the numeric provider codes behind it are not part of the
// contract.
type Category string

const (
	// CatAuthRequired means the session cookie is dead. The only correct action
	// is to ask the user for a new one; retrying will not help.
	CatAuthRequired Category = "auth_required"
	// CatPermissionDenied means the account may not touch this object. Retrying
	// with the same credentials cannot succeed.
	CatPermissionDenied Category = "permission_denied"
	// CatObjectNotFound means the addressed object does not exist.
	CatObjectNotFound Category = "object_not_found"
	// CatParentNotFound means an intermediate directory in a path is missing.
	CatParentNotFound Category = "parent_not_found"
	// CatTargetConflict means the destination already exists and the operation
	// refuses to clobber it. The existing object must be preserved.
	CatTargetConflict Category = "target_conflict"
	// CatLocked means another client holds a lock on the resource.
	CatLocked Category = "locked"
	// CatNotSupported means the backend has no such capability. This is a
	// permanent property of the operation, not a transient failure.
	CatNotSupported Category = "operation_not_supported"
	// CatUpstreamTransient means the provider was briefly unavailable. The same
	// request may succeed later.
	CatUpstreamTransient Category = "upstream_transient"
	// CatRateLimited means the provider is throttling. Backing off is required.
	CatRateLimited Category = "rate_limited"
	// CatQuotaExceeded means the account is out of space.
	CatQuotaExceeded Category = "quota_exceeded"
	// CatInvalidRequest means the request itself was malformed. Retrying the
	// same request cannot succeed.
	CatInvalidRequest Category = "invalid_request"
	// CatInternal means this service failed in a way that is its own fault.
	CatInternal Category = "internal_error"
	// CatCancelled means the client went away or the context was cancelled.
	CatCancelled Category = "cancelled"
	// CatUnknown means nothing matched. It is deliberately distinct from the
	// others so an unclassified failure is visible rather than silently folded
	// into a category that implies a remedy.
	CatUnknown Category = "unknown"
)

// Error is a failure carrying its category.
//
// The original provider error is preserved so a caller that needs the raw
// status or code can still reach it, but callers are expected to switch on
// Category.
type Error struct {
	// Category is the stable class.
	Category Category
	// Op names the operation that failed, for logs and diagnostics.
	Op string
	// Detail is a short human-readable explanation that must never contain a
	// credential or a signed URL query string.
	Detail string
	// Err is the underlying cause, if any.
	Err error
}

// Error renders the failure without leaking the underlying provider message
// when that message might carry a credential.
func (e *Error) Error() string {
	msg := string(e.Category)
	if e.Op != "" {
		msg = e.Op + ": " + msg
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if e.Err != nil && e.Detail == "" {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap exposes the cause for errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Err }

// Is reports equality against another Error of the same category, so
// errors.Is can match on the category alone.
func (e *Error) Is(target error) bool {
	var t *Error
	if errors.As(target, &t) {
		return e.Category == t.Category
	}
	return false
}

// New builds a categorised error.
func New(cat Category, op, detail string) *Error {
	return &Error{Category: cat, Op: op, Detail: detail}
}

// Wrap attaches a cause to a categorised error.
func Wrap(cat Category, op string, err error) *Error {
	return &Error{Category: cat, Op: op, Err: err}
}

// CategoryOf extracts the category from an error, defaulting to CatUnknown.
//
// A nil error yields an empty category so a caller can test success without a
// separate nil check.
func CategoryOf(err error) Category {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Category
	}
	return CatUnknown
}

// IsCategory reports whether err carries the given category.
func IsCategory(err error, cat Category) bool {
	return CategoryOf(err) == cat
}

// statusFor maps a category to the HTTP status a downstream client should see.
//
// The mapping is intentionally conservative: anything that is not clearly a
// client-visible condition becomes 502 rather than 500, because a 500 tells the
// client "do not retry" while a provider outage is exactly when retrying helps.
func statusFor(cat Category) int {
	switch cat {
	case CatAuthRequired:
		return http.StatusUnauthorized
	case CatPermissionDenied:
		return http.StatusForbidden
	case CatObjectNotFound, CatParentNotFound:
		return http.StatusNotFound
	case CatTargetConflict:
		return http.StatusConflict
	case CatLocked:
		return http.StatusLocked
	case CatNotSupported:
		return http.StatusMethodNotAllowed
	case CatRateLimited:
		return http.StatusTooManyRequests
	case CatQuotaExceeded:
		return http.StatusInsufficientStorage
	case CatInvalidRequest:
		return http.StatusBadRequest
	case CatCancelled:
		// The client is gone; 499 is nginx's convention for that.
		return 499
	case CatUpstreamTransient, CatInternal, CatUnknown:
		return http.StatusBadGateway
	}
	return http.StatusBadGateway
}

// StatusCode returns the HTTP status a downstream client should receive for
// err. A nil error yields 200.
func StatusCode(err error) int {
	if err == nil {
		return http.StatusOK
	}
	return statusFor(CategoryOf(err))
}

// Errno returns the errno a mount client should surface for a category (ADR
// 0010). It is the third leg of the category contract: one category, decided
// once from the provider's response, maps outward to both an HTTP status and
// an errno, so the WebDAV endpoint's COPY answer and rclone/VFS's ENOSYS can
// never disagree about what the backend meant.
//
// The mapping is about remedy, not similarity: a category whose only remedy is
// "report it" lands on the errno that makes the caller stop retrying.
func Errno(cat Category) syscall.Errno {
	switch cat {
	case CatAuthRequired:
		// A dead session is a credential problem; the caller cannot fix it by
		// retrying and must not confuse it with a missing object.
		return syscall.EACCES
	case CatPermissionDenied:
		return syscall.EACCES
	case CatObjectNotFound, CatParentNotFound:
		return syscall.ENOENT
	case CatTargetConflict:
		// The existing object must be preserved; EEXIST is the errno whose
		// contract says exactly that.
		return syscall.EEXIST
	case CatLocked:
		return syscall.EBUSY
	case CatNotSupported:
		return syscall.ENOSYS
	case CatRateLimited, CatUpstreamTransient:
		return syscall.EAGAIN
	case CatQuotaExceeded:
		return syscall.ENOSPC
	case CatInvalidRequest:
		return syscall.EINVAL
	case CatCancelled:
		return syscall.EINTR
	}
	// Internal, unknown and any category added later fall through to EIO: the
	// last-resort errno of ADR 0010. A zero errno would read as success.
	return syscall.EIO
}

// ErrnoOf maps an error through its category. An unclassified error is EIO,
// matching Errno's fallback.
func ErrnoOf(err error) syscall.Errno { return Errno(CategoryOf(err)) }

// Permanent reports whether retrying the same request can ever succeed.
//
// A supervisor uses this to decide between "back off and retry" and "surface
// the failure to the user". Getting it wrong in the optimistic direction turns
// a dead credential into an endless retry loop; getting it wrong in the
// pessimistic direction turns a transient outage into a spurious reauth
// prompt.
func Permanent(err error) bool {
	switch CategoryOf(err) {
	case CatAuthRequired, CatPermissionDenied, CatObjectNotFound,
		CatParentNotFound, CatTargetConflict, CatNotSupported,
		CatQuotaExceeded, CatInvalidRequest:
		return true
	}
	return false
}

// NeedsReauth reports whether the failure means the user must supply a new
// credential. It is the single condition that moves the mount supervisor into
// NeedsReauth (ADR 0003).
func NeedsReauth(err error) bool {
	return IsCategory(err, CatAuthRequired)
}

// String renders the category for logs and diagnostics.
func (c Category) String() string { return string(c) }

// NotSupported builds an operation_not_supported error naming the operation.
func NotSupported(op string) *Error {
	return &Error{
		Category: CatNotSupported,
		Op:       op,
		Detail:   fmt.Sprintf("%s is not supported by the Quark backend", op),
	}
}

// IsNotSupported reports whether err means an unsupported operation.
func IsNotSupported(err error) bool {
	return IsCategory(err, CatNotSupported)
}
