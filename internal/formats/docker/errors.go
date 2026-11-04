package docker

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// OCI distribution error codes (distribution-spec, "Error Codes").
const (
	codeBlobUnknown         = "BLOB_UNKNOWN"
	codeBlobUploadInvalid   = "BLOB_UPLOAD_INVALID"
	codeBlobUploadUnknown   = "BLOB_UPLOAD_UNKNOWN"
	codeDigestInvalid       = "DIGEST_INVALID"
	codeManifestBlobUnknown = "MANIFEST_BLOB_UNKNOWN"
	codeManifestInvalid     = "MANIFEST_INVALID"
	codeManifestUnknown     = "MANIFEST_UNKNOWN"
	codeNameInvalid         = "NAME_INVALID"
	codeNameUnknown         = "NAME_UNKNOWN"
	codeSizeInvalid         = "SIZE_INVALID"
	codeUnauthorized        = "UNAUTHORIZED"
	codeDenied              = "DENIED"
	codeUnsupported         = "UNSUPPORTED"
	codeTooManyRequests     = "TOOMANYREQUESTS"
	// Not in the spec table but understood by every client (distribution uses them).
	codeUnavailable       = "UNAVAILABLE"
	codePaginationInvalid = "PAGINATION_NUMBER_INVALID"
	codeUnknown           = "UNKNOWN"
)

type ociError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

type ociErrors struct {
	Errors []ociError `json:"errors"`
}

// writeError answers with the OCI error envelope.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeErrorDetail(w, status, code, msg, nil)
}

func writeErrorDetail(w http.ResponseWriter, status int, code, msg string, detail any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Del("Content-Length")
	h.Del("Docker-Content-Digest")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ociErrors{Errors: []ociError{{Code: code, Message: msg, Detail: detail}}})
}

// writeCoreError maps a core sentinel error to the OCI envelope. notFound is
// the code to use for core.ErrNotFound (BLOB_UNKNOWN, MANIFEST_UNKNOWN, ...).
// Authentication errors are answered by the caller (they need a challenge).
func writeCoreError(w http.ResponseWriter, err error, notFound string) {
	status := core.HTTPStatus(err)
	code := codeUnknown
	switch {
	case errors.Is(err, core.ErrNotFound):
		code = notFound
	case errors.Is(err, core.ErrDigestMismatch):
		code = codeDigestInvalid
	case errors.Is(err, core.ErrWriteDenied), errors.Is(err, core.ErrForbidden), errors.Is(err, core.ErrQuarantined):
		code = codeDenied
	case errors.Is(err, core.ErrUnauthenticated):
		code = codeUnauthorized
	case errors.Is(err, core.ErrInvalid):
		code = codeManifestInvalid
		if notFound == codeBlobUnknown || notFound == codeBlobUploadUnknown {
			code = codeBlobUploadInvalid
		}
	case errors.Is(err, core.ErrOffline), errors.Is(err, core.ErrUpstream):
		code = codeUnavailable
	case errors.Is(err, core.ErrNotImplemented):
		code = codeUnsupported
	}
	msg := err.Error()
	if status >= 500 {
		// Do not leak internals (paths, driver errors) to clients.
		msg = http.StatusText(status)
	}
	writeError(w, status, code, msg)
}
