package entitysdk

import (
	"errors"
	"testing"

	"go.entitychurch.org/entity-core-go/core/types"
)

func TestErrorFormat(t *testing.T) {
	e := NewError(404, "not_found", "path not found: foo")
	got := e.Error()
	want := "sdk error 404 (not_found): path not found: foo"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestErrorUnwrap(t *testing.T) {
	cause := errors.New("underlying")
	e := WrapError(500, "store_failed", "wrap test", cause)
	if !errors.Is(e, cause) {
		t.Error("errors.Is did not see wrapped cause")
	}
}

func TestStatusOf(t *testing.T) {
	if got := StatusOf(nil); got != 0 {
		t.Errorf("StatusOf(nil) = %d, want 0", got)
	}
	if got := StatusOf(errors.New("plain")); got != 0 {
		t.Errorf("StatusOf(plain) = %d, want 0", got)
	}
	if got := StatusOf(NewError(404, "nf", "")); got != 404 {
		t.Errorf("StatusOf(404) = %d, want 404", got)
	}
}

func TestStatusPredicates(t *testing.T) {
	cases := []struct {
		status                                                                         uint
		notFound, forbidden, conflict, rateLimited, notSupported, client, auth, system bool
	}{
		{400, false, false, false, false, false, true, false, false},
		{403, false, true, false, false, false, false, true, false},
		{404, true, false, false, false, false, true, false, false},
		{409, false, false, true, false, false, true, false, false},
		{429, false, false, false, true, false, true, false, false},
		{500, false, false, false, false, false, false, false, true},
		{501, false, false, false, false, true, false, false, true},
	}
	for _, c := range cases {
		e := NewError(c.status, "", "")
		check := func(name string, got, want bool) {
			if got != want {
				t.Errorf("status %d: %s = %v, want %v", c.status, name, got, want)
			}
		}
		check("IsNotFound", IsNotFound(e), c.notFound)
		check("IsForbidden", IsForbidden(e), c.forbidden)
		check("IsConflict", IsConflict(e), c.conflict)
		check("IsRateLimited", IsRateLimited(e), c.rateLimited)
		check("IsNotSupported", IsNotSupported(e), c.notSupported)
		check("IsClientError", IsClientError(e), c.client)
		check("IsAuthError", IsAuthError(e), c.auth)
		check("IsSystemError", IsSystemError(e), c.system)
	}
}

func TestErrorFromResponseNilOrSuccess(t *testing.T) {
	if got := ErrorFromResponse(nil); got != nil {
		t.Errorf("nil response → %v, want nil", got)
	}
	if got := ErrorFromResponse(&Response{Status: 200}); got != nil {
		t.Errorf("200 response → %v, want nil", got)
	}
	if got := ErrorFromResponse(&Response{Status: 207}); got != nil {
		t.Errorf("207 response → %v, want nil (partial success is not failure)", got)
	}
}

func TestErrorFromResponseWithErrorEntity(t *testing.T) {
	ent, err := types.ErrorData{Code: "capability_denied", Message: "nope"}.ToEntity()
	if err != nil {
		t.Fatal(err)
	}
	resp := &Response{Status: 403, Type: ent.Type, Data: ent.Data, Hash: ent.ContentHash}
	e := ErrorFromResponse(resp)
	if e == nil {
		t.Fatal("ErrorFromResponse returned nil for 403")
	}
	if e.Status != 403 || e.Code != "capability_denied" || e.Message != "nope" {
		t.Errorf("unexpected error: %+v", e)
	}
}

func TestErrorFromResponseSynthesisesDefaults(t *testing.T) {
	resp := &Response{Status: 500} // no error entity body
	e := ErrorFromResponse(resp)
	if e == nil || e.Status != 500 || e.Code != "internal_error" {
		t.Errorf("got %+v, want status=500 code=internal_error", e)
	}
}

func TestStoreErrorsAreTyped(t *testing.T) {
	st := newTestStore()
	// entity.NewEntity rejects an empty type string.
	_, err := st.Put("bad", "", 1)
	if err == nil {
		t.Fatal("expected error for empty type")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err is not *Error: %T %v", err, err)
	}
	if e.Status != 400 {
		t.Errorf("want 400, got %d", e.Status)
	}
}
