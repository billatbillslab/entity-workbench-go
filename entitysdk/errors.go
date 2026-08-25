package entitysdk

import (
	"errors"
	"fmt"

	"go.entitychurch.org/entity-core-go/core/types"
)

// Error is the SDK's typed error. It carries the HTTP-style status
// code, machine-readable code, and human-readable message from
// SDK-OPERATIONS §12.1 / §12.2.
//
// Status codes MUST be preserved across the SDK boundary per §12.3.
// Use StatusOf, IsNotFound, IsForbidden, etc. to inspect errors
// programmatically; errors.As(err, &sdkErr) also works.
type Error struct {
	Status  uint   // 400, 403, 404, 409, 429, 500, 501, ...
	Code    string // machine-readable ("not_found", "capability_denied", ...)
	Message string // human-readable
	Cause   error  // wrapped underlying error, if any
}

func (e *Error) Error() string {
	base := fmt.Sprintf("sdk error %d (%s)", e.Status, e.Code)
	if e.Message != "" {
		base = base + ": " + e.Message
	}
	if e.Cause != nil && e.Cause.Error() != e.Message {
		base = base + ": " + e.Cause.Error()
	}
	return base
}

func (e *Error) Unwrap() error { return e.Cause }

// NewError constructs an Error with no wrapped cause.
func NewError(status uint, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// WrapError wraps an underlying error with an SDK status and code.
// If message is empty, cause.Error() is used.
func WrapError(status uint, code, message string, cause error) *Error {
	if message == "" && cause != nil {
		message = cause.Error()
	}
	return &Error{Status: status, Code: code, Message: message, Cause: cause}
}

// ErrorFromResponse converts a non-2xx SDK Response into an Error.
// If the response entity is system/protocol/error, its code and
// message are used; otherwise a generic message is synthesized from
// the status. Returns nil if resp is nil or Status < 400.
func ErrorFromResponse(resp *Response) *Error {
	if resp == nil || resp.Status < 400 {
		return nil
	}
	code, message := "", ""
	if resp.Type == types.TypeError && len(resp.Data) > 0 {
		if data, err := types.ErrorDataFromEntity(resp.Entity()); err == nil {
			code = data.Code
			message = data.Message
		}
	}
	if code == "" {
		code = defaultCodeForStatus(resp.Status)
	}
	if message == "" {
		message = fmt.Sprintf("status %d", resp.Status)
	}
	return &Error{Status: resp.Status, Code: code, Message: message}
}

func defaultCodeForStatus(status uint) string {
	switch status {
	case 400:
		return "bad_request"
	case 403:
		return "forbidden"
	case 404:
		return "not_found"
	case 409:
		return "conflict"
	case 429:
		return "rate_limited"
	case 500:
		return "internal_error"
	case 501:
		return "not_supported"
	default:
		return "error"
	}
}

// StatusOf returns the status code carried by err, or 0 if err is not
// an SDK Error (or is nil).
func StatusOf(err error) uint {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// CodeOf returns the machine-readable code carried by err, or "" if
// err is not an SDK Error (or is nil). The status-code sibling of
// StatusOf: status says which class of failure, code says which one.
// Callers distinguishing between two failures that share a status —
// several of the published-root verification refusals are 403 — need
// the code, not the status.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// IsStatus reports whether err is an SDK Error with the given status.
func IsStatus(err error, status uint) bool {
	return StatusOf(err) == status
}

// IsNotFound reports whether err is a 404.
func IsNotFound(err error) bool { return IsStatus(err, 404) }

// IsForbidden reports whether err is a 403. Per SDK-OPERATIONS §12.3,
// authorization errors are distinct from other client errors.
func IsForbidden(err error) bool { return IsStatus(err, 403) }

// IsConflict reports whether err is a 409 (CAS failure, merge conflict).
func IsConflict(err error) bool { return IsStatus(err, 409) }

// IsRateLimited reports whether err is a 429.
func IsRateLimited(err error) bool { return IsStatus(err, 429) }

// IsNotSupported reports whether err is a 501 (handler does not support
// the requested operation).
func IsNotSupported(err error) bool { return IsStatus(err, 501) }

// IsClientError reports whether err is a 4xx other than 403. Per
// SDK-OPERATIONS §12.3, these are errors the caller can fix.
func IsClientError(err error) bool {
	s := StatusOf(err)
	return s >= 400 && s < 500 && s != 403
}

// IsAuthError reports whether err is a 403. Separate from IsClientError
// because capability problems need different handling than bad input.
func IsAuthError(err error) bool { return IsStatus(err, 403) }

// IsSystemError reports whether err is a 5xx.
func IsSystemError(err error) bool {
	s := StatusOf(err)
	return s >= 500 && s < 600
}
