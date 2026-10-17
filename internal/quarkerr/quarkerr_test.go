package quarkerr

import (
	"errors"
	"net/http"
	"syscall"
	"testing"
)

func TestErrorRendering(t *testing.T) {
	cases := []struct {
		name string
		err  *Error
		want string
	}{
		{
			name: "category only",
			err:  &Error{Category: CatAuthRequired},
			want: "auth_required",
		},
		{
			name: "op and category",
			err:  &Error{Category: CatAuthRequired, Op: "verify"},
			want: "verify: auth_required",
		},
		{
			name: "detail hides cause",
			err: &Error{
				Category: CatPermissionDenied,
				Op:       "list",
				Detail:   "scope refused",
				Err:      errors.New("secret cause"),
			},
			want: "list: permission_denied: scope refused",
		},
		{
			name: "cause shown when no detail",
			err:  Wrap(CatUpstreamTransient, "list", errors.New("boom")),
			want: "list: upstream_transient: boom",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCategoryOf(t *testing.T) {
	if got := CategoryOf(nil); got != "" {
		t.Fatalf("CategoryOf(nil) = %q, want empty", got)
	}
	if got := CategoryOf(errors.New("plain")); got != CatUnknown {
		t.Fatalf("CategoryOf(plain) = %q, want %q", got, CatUnknown)
	}
	wrapped := Wrap(CatRateLimited, "list", errors.New("cause"))
	if got := CategoryOf(wrapped); got != CatRateLimited {
		t.Fatalf("CategoryOf(wrapped) = %q, want %q", got, CatRateLimited)
	}
}

func TestIsCategoryAndErrorsIs(t *testing.T) {
	err := Wrap(CatTargetConflict, "upload", errors.New("same name"))
	if !IsCategory(err, CatTargetConflict) {
		t.Fatal("IsCategory should match the wrapped category")
	}
	if !errors.Is(err, New(CatTargetConflict, "", "")) {
		t.Fatal("errors.Is should match another error of the same category")
	}
	if errors.Is(err, New(CatAuthRequired, "", "")) {
		t.Fatal("errors.Is must not match a different category")
	}
}

func TestStatusCode(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, http.StatusOK},
		{New(CatAuthRequired, "", ""), http.StatusUnauthorized},
		{New(CatPermissionDenied, "", ""), http.StatusForbidden},
		{New(CatObjectNotFound, "", ""), http.StatusNotFound},
		{New(CatParentNotFound, "", ""), http.StatusNotFound},
		{New(CatTargetConflict, "", ""), http.StatusConflict},
		{New(CatLocked, "", ""), http.StatusLocked},
		{New(CatNotSupported, "", ""), http.StatusMethodNotAllowed},
		{New(CatRateLimited, "", ""), http.StatusTooManyRequests},
		{New(CatQuotaExceeded, "", ""), http.StatusInsufficientStorage},
		{New(CatInvalidRequest, "", ""), http.StatusBadRequest},
		{New(CatUpstreamTransient, "", ""), http.StatusBadGateway},
		{New(CatInternal, "", ""), http.StatusBadGateway},
		{New(CatUnknown, "", ""), http.StatusBadGateway},
		{New(CatCancelled, "", ""), 499},
	}
	for _, tc := range cases {
		if got := StatusCode(tc.err); got != tc.want {
			t.Fatalf("StatusCode(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestPermanent(t *testing.T) {
	permanent := []Category{
		CatAuthRequired, CatPermissionDenied, CatObjectNotFound,
		CatParentNotFound, CatTargetConflict, CatNotSupported,
		CatQuotaExceeded, CatInvalidRequest,
	}
	for _, cat := range permanent {
		if !Permanent(New(cat, "", "")) {
			t.Fatalf("Permanent(%s) = false, want true", cat)
		}
	}
	transient := []Category{
		CatUpstreamTransient, CatRateLimited, CatLocked, CatInternal,
		CatCancelled, CatUnknown,
	}
	for _, cat := range transient {
		if Permanent(New(cat, "", "")) {
			t.Fatalf("Permanent(%s) = true, want false", cat)
		}
	}
}

func TestNeedsReauth(t *testing.T) {
	if !NeedsReauth(New(CatAuthRequired, "", "")) {
		t.Fatal("auth_required must need reauth")
	}
	if NeedsReauth(New(CatUpstreamTransient, "", "")) {
		t.Fatal("upstream_transient must not need reauth")
	}
}

func TestNotSupported(t *testing.T) {
	err := NotSupported("copy")
	if !IsNotSupported(err) {
		t.Fatal("IsNotSupported should recognise a NotSupported error")
	}
	if StatusCode(err) != http.StatusMethodNotAllowed {
		t.Fatalf("copy should map to 405, got %d", StatusCode(err))
	}
	if !Permanent(err) {
		t.Fatal("operation_not_supported must be permanent")
	}
}

// TestThreeWayMapping is the issue-22 contract table: every category maps to
// one HTTP status and one errno, both decided here so the WebDAV endpoint and
// the rclone/VFS layer can never drift apart. The ADR 0010 rows are explicit.
func TestThreeWayMapping(t *testing.T) {
	cases := []struct {
		cat    Category
		status int
		errno  syscall.Errno
	}{
		{CatAuthRequired, http.StatusUnauthorized, syscall.EACCES},
		{CatPermissionDenied, http.StatusForbidden, syscall.EACCES},
		{CatObjectNotFound, http.StatusNotFound, syscall.ENOENT},
		{CatParentNotFound, http.StatusNotFound, syscall.ENOENT},
		{CatTargetConflict, http.StatusConflict, syscall.EEXIST},
		{CatLocked, http.StatusLocked, syscall.EBUSY},
		{CatNotSupported, http.StatusMethodNotAllowed, syscall.ENOSYS},
		{CatRateLimited, http.StatusTooManyRequests, syscall.EAGAIN},
		{CatUpstreamTransient, http.StatusBadGateway, syscall.EAGAIN},
		{CatQuotaExceeded, http.StatusInsufficientStorage, syscall.ENOSPC},
		{CatInvalidRequest, http.StatusBadRequest, syscall.EINVAL},
		{CatCancelled, 499, syscall.EINTR},
		{CatInternal, http.StatusBadGateway, syscall.EIO},
		{CatUnknown, http.StatusBadGateway, syscall.EIO},
	}
	for _, tc := range cases {
		if got := StatusCode(New(tc.cat, "", "")); got != tc.status {
			t.Errorf("%s: StatusCode = %d, want %d", tc.cat, got, tc.status)
		}
		if got := Errno(tc.cat); got != tc.errno {
			t.Errorf("%s: Errno = %v, want %v", tc.cat, got, tc.errno)
		}
		if got := ErrnoOf(New(tc.cat, "", "")); got != tc.errno {
			t.Errorf("%s: ErrnoOf = %v, want %v", tc.cat, got, tc.errno)
		}
	}
	// The fallback never fabricates a zero errno: an unclassified error must
	// read as a failure, never as success.
	if got := ErrnoOf(errors.New("plain")); got != syscall.EIO {
		t.Errorf("ErrnoOf(plain) = %v, want EIO", got)
	}
}
