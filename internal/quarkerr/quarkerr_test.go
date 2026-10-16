package quarkerr

import (
	"errors"
	"net/http"
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
