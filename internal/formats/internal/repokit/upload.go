package repokit

import (
	"context"
	"crypto/md5"  //nolint:gosec // APT indexes list MD5sum next to SHA256
	"crypto/sha1" //nolint:gosec // APT indexes list SHA1 next to SHA256
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

const (
	maxField  = 4 << 10
	maxFields = 64
)

// Upload is a received package file, already in the blob store.
type Upload struct {
	Filename string // client file name (base name), may be empty for raw bodies
	Digest   core.Digest
	Size     int64
	// Checksums are md5, sha1 and sha512 (sha256 is the digest), hex.
	Checksums map[string]string
	// Fields are the form fields (multipart) or query parameters (raw body).
	Fields map[string]string
}

// Field returns the first non-empty value among the named fields.
func (u *Upload) Field(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(u.Fields[n]); v != "" {
			return v
		}
	}
	return ""
}

// ReceiveUpload stores the package file of r in bs: the request body itself
// (PUT, or POST with a non-multipart body) or the first file part of a
// multipart/form-data body. Query parameters are fields too; form fields
// override them.
func ReceiveUpload(ctx context.Context, r *http.Request, bs core.BlobStore) (*Upload, error) {
	u := &Upload{Fields: map[string]string{}}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			u.Fields[k] = v[0]
		}
	}
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "multipart/form-data" {
		if err := u.store(ctx, bs, r.Body); err != nil {
			return nil, err
		}
		return u, nil
	}
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, fmt.Errorf("multipart body: %w: %w", err, core.ErrInvalid)
	}
	stored := false
	for n := 0; ; n++ {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read multipart body: %w: %w", err, core.ErrInvalid)
		}
		if n > maxFields {
			return nil, fmt.Errorf("too many form parts: %w", core.ErrInvalid)
		}
		if part.FileName() != "" {
			if stored {
				return nil, fmt.Errorf("upload one package file per request: %w", core.ErrInvalid)
			}
			u.Filename = path.Base(strings.ReplaceAll(part.FileName(), "\\", "/"))
			if err := u.store(ctx, bs, part); err != nil {
				return nil, err
			}
			stored = true
			continue
		}
		b, err := io.ReadAll(io.LimitReader(part, maxField+1))
		if err != nil || len(b) > maxField {
			return nil, fmt.Errorf("form field %q is too large or unreadable: %w", part.FormName(), core.ErrInvalid)
		}
		u.Fields[part.FormName()] = string(b)
	}
	if !stored {
		return nil, fmt.Errorf("the multipart body has no file part: %w", core.ErrInvalid)
	}
	return u, nil
}

func (u *Upload) store(ctx context.Context, bs core.BlobStore, body io.Reader) error {
	m, s1, s5 := md5.New(), sha1.New(), sha512.New() //nolint:gosec // see imports
	d, n, err := bs.Put(ctx, io.TeeReader(body, io.MultiWriter(m, s1, s5)))
	if err != nil {
		return fmt.Errorf("store upload: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("empty upload: %w", core.ErrInvalid)
	}
	u.Digest, u.Size = d, n
	u.Checksums = map[string]string{
		"md5": hex.EncodeToString(m.Sum(nil)), "sha1": hex.EncodeToString(s1.Sum(nil)), "sha512": hex.EncodeToString(s5.Sum(nil)),
	}
	return nil
}
