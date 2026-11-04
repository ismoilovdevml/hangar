package pypi

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // md5_digest is part of the upload protocol, verified for integrity only
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/crypto/blake2b"

	"github.com/ismoilovdevml/hangar/internal/core"
)

const (
	maxFieldBytes     = 8 << 20  // one form field (long descriptions)
	maxFormBytes      = 32 << 20 // all non-file fields together
	maxMetadataBytes  = 16 << 20 // a wheel's METADATA
	maxDescriptionLen = 64 << 10 // description kept on the component
	fieldContent      = "content"
)

// multiValued are core metadata fields sent once per value.
var multiValued = map[string]bool{
	"classifiers": true, "requires_dist": true, "provides_dist": true, "obsoletes_dist": true,
	"requires_external": true, "project_urls": true, "provides_extra": true, "dynamic": true,
	"license_files": true, "license_file": true, "platform": true, "supported_platform": true,
	"requires": true, "provides": true, "obsoletes": true,
}

// protocolFields are upload form fields that are not core metadata.
var protocolFields = map[string]bool{
	":action": true, "protocol_version": true, fieldContent: true, "md5_digest": true,
	"sha256_digest": true, "blake2_256_digest": true, "gpg_signature": true, "attestations": true,
	"filetype": true, "pyversion": true, "comment": true,
}

// stored is an uploaded file after it reached the blob store.
type stored struct {
	filename string
	digest   core.Digest
	size     int64
	md5      []byte
	blake2   []byte
}

// writeTarget resolves the repository an upload to repo is stored in.
func (h *handler) writeTarget(ctx context.Context, repo *core.Repository) (*core.Repository, error) {
	switch repo.Type {
	case core.RepoHosted:
		return repo, nil
	case core.RepoGroup:
		if repo.Config.Group == nil || repo.Config.Group.WriteMember == "" {
			return nil, nil
		}
		m, err := h.deps.Repos.Get(ctx, repo.Config.Group.WriteMember)
		if err != nil {
			return nil, err
		}
		if m.Type != core.RepoHosted || m.Format != Name {
			return nil, fmt.Errorf("group %q write member %q is not a hosted %s repository: %w", repo.Name, m.Name, Name, core.ErrInvalid)
		}
		return m, nil
	}
	return nil, nil
}

// upload implements the legacy upload API (POST multipart/form-data with
// ":action=file_upload"), as used by twine, uv publish and poetry publish.
func (h *handler) upload(w http.ResponseWriter, r *http.Request) {
	repo := core.RepositoryFrom(r.Context())
	target, err := h.writeTarget(r.Context(), repo)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if target == nil {
		w.Header().Set("Allow", "GET, HEAD")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = fmt.Fprintf(w, "%s repository %s does not accept uploads\n", repo.Type, repo.Name)
		return
	}
	ctx, ok := h.authorize(w, r, target, core.ActionWrite)
	if !ok {
		return
	}
	if target != repo { // a group write: the client must also be allowed to write to the group
		if err := h.deps.AuthZ.Authorize(ctx, core.PrincipalFrom(ctx), core.ActionWrite, core.RepoResource(repo.Name)); err != nil {
			h.writeError(w, r, err)
			return
		}
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, target)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	form, file, err := readUploadForm(ctx, r, bs)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	comp, assets, err := h.prepareUpload(ctx, bs, form, file)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if _, _, err := h.deps.Components.Publish(ctx, target, core.PublishRequest{Component: comp, Assets: assets}); err != nil {
		h.writeError(w, r, err)
		return
	}
	h.log.InfoContext(ctx, "pypi upload", "repo", target.Name, "project", comp.Name, "version", comp.Version, "file", file.filename)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "OK\n")
}

// readUploadForm streams the multipart body: fields are kept (bounded), the
// "content" file goes straight to the blob store while md5 and blake2b-256
// are computed alongside the store's sha256.
func readUploadForm(ctx context.Context, r *http.Request, bs core.BlobStore) (map[string][]string, *stored, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, nil, fmt.Errorf("upload must be multipart/form-data: %w", core.ErrInvalid)
	}
	form := map[string][]string{}
	var file *stored
	total := 0
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read multipart body: %w: %w", err, core.ErrInvalid)
		}
		name := part.FormName()
		switch {
		case name == fieldContent:
			if file != nil {
				return nil, nil, fmt.Errorf("more than one %q part: %w", fieldContent, core.ErrInvalid)
			}
			if a := first(form, ":action"); a != "" && a != "file_upload" {
				return nil, nil, fmt.Errorf("unsupported :action %q: %w", a, core.ErrInvalid)
			}
			file, err = storeContent(ctx, bs, part)
			if err != nil {
				return nil, nil, err
			}
		case part.FileName() != "": // gpg_signature and other side files are not kept
			if _, err := io.Copy(io.Discard, part); err != nil {
				return nil, nil, fmt.Errorf("read %q: %w: %w", name, err, core.ErrInvalid)
			}
		default:
			b, err := io.ReadAll(io.LimitReader(part, maxFieldBytes+1))
			if err != nil {
				return nil, nil, fmt.Errorf("read field %q: %w: %w", name, err, core.ErrInvalid)
			}
			if len(b) > maxFieldBytes {
				return nil, nil, fmt.Errorf("field %q is larger than %d bytes: %w", name, maxFieldBytes, core.ErrInvalid)
			}
			total += len(b)
			if total > maxFormBytes {
				return nil, nil, fmt.Errorf("form fields exceed %d bytes: %w", maxFormBytes, core.ErrInvalid)
			}
			form[name] = append(form[name], string(b))
		}
		_ = part.Close()
	}
	if a := first(form, ":action"); a != "file_upload" {
		return nil, nil, fmt.Errorf("unsupported :action %q (only file_upload): %w", a, core.ErrInvalid)
	}
	if v := first(form, "protocol_version"); v != "" && v != "1" {
		return nil, nil, fmt.Errorf("unsupported protocol_version %q: %w", v, core.ErrInvalid)
	}
	if file == nil {
		return nil, nil, fmt.Errorf("missing %q file: %w", fieldContent, core.ErrInvalid)
	}
	return form, file, nil
}

func storeContent(ctx context.Context, bs core.BlobStore, part *multipart.Part) (*stored, error) {
	filename := part.FileName()
	if !safeFilename(filename) {
		return nil, fmt.Errorf("filename %q is not allowed: %w", filename, core.ErrInvalid)
	}
	md5h := md5.New() //nolint:gosec // protocol digest, see import
	b2h, err := blake2b.New256(nil)
	if err != nil {
		return nil, fmt.Errorf("blake2b: %w", err)
	}
	d, n, err := bs.Put(ctx, io.TeeReader(part, io.MultiWriter(md5h, b2h)))
	if err != nil {
		return nil, fmt.Errorf("store %s: %w", filename, err)
	}
	return &stored{filename: filename, digest: d, size: n, md5: md5h.Sum(nil), blake2: b2h.Sum(nil)}, nil
}

// prepareUpload validates the form against the stored file and builds the
// publish request: the release component and the file (plus, for wheels, the
// PEP 658 metadata file).
func (h *handler) prepareUpload(ctx context.Context, bs core.BlobStore, form map[string][]string, file *stored) (*core.Component, []*core.Asset, error) {
	name, version, filetype := first(form, "name"), first(form, "version"), first(form, "filetype")
	if !ValidProjectName(name) {
		return nil, nil, fmt.Errorf("invalid or missing project name %q: %w", name, core.ErrInvalid)
	}
	if version == "" || len(version) > 128 || strings.ContainsAny(version, "/\\ ") {
		return nil, nil, fmt.Errorf("invalid or missing version %q: %w", version, core.ErrInvalid)
	}
	if err := verifyDigests(form, file); err != nil {
		return nil, nil, err
	}
	project := Normalize(name)
	fileVersion, kind, err := splitFilename(file.filename, project)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case filetype != string(kindWheel) && filetype != string(kindSdist):
		return nil, nil, fmt.Errorf("unsupported filetype %q (want sdist or bdist_wheel): %w", filetype, core.ErrInvalid)
	case string(kind) != filetype:
		return nil, nil, fmt.Errorf("filename %q is not a %s: %w", file.filename, filetype, core.ErrInvalid)
	case !sameVersion(fileVersion, version):
		return nil, nil, fmt.Errorf("filename %q does not match version %q: %w", file.filename, version, core.ErrInvalid)
	}

	requiresPython := first(form, "requires_python")
	path := filePath(project, file.filename)
	assets := []*core.Asset{{
		Path: path, Kind: core.AssetContent, BlobDigest: file.digest, Size: file.size,
		ContentType: contentType(file.filename),
		Checksums:   map[string]string{"md5": hex.EncodeToString(file.md5), "blake2b_256": hex.EncodeToString(file.blake2)},
	}}
	if kind == kindWheel {
		meta, err := h.extractWheelMetadata(ctx, bs, file, project)
		if err != nil {
			return nil, nil, err
		}
		if requiresPython == "" {
			requiresPython = metadataHeader(meta.body, "Requires-Python")
		}
		assets = append(assets, &core.Asset{
			Path: path + metadataSuffix, Kind: core.AssetMetadata, BlobDigest: meta.digest, Size: meta.size,
			ContentType: "text/plain; charset=utf-8",
			Attributes:  map[string]any{"filename": file.filename + metadataSuffix, "metadata_for": file.filename},
		})
	}
	assets[0].Attributes = map[string]any{
		"filename": file.filename, "filetype": filetype, "pyversion": first(form, "pyversion"),
		"requires_python": requiresPython,
	}
	comp := &core.Component{Name: project, Version: version, Attributes: map[string]any{
		"display_name":    name,
		"summary":         first(form, "summary"),
		"requires_python": requiresPython,
		"metadata":        metadataAttributes(form),
	}}
	return comp, assets, nil
}

// verifyDigests checks every digest the client declared; at least one is required.
func verifyDigests(form map[string][]string, file *stored) error {
	declared := false
	if v := strings.ToLower(first(form, "sha256_digest")); v != "" {
		declared = true
		if v != file.digest.Hex() {
			return fmt.Errorf("sha256_digest does not match the uploaded file: %w", core.ErrDigestMismatch)
		}
	}
	if v := first(form, "md5_digest"); v != "" {
		declared = true
		// twine sends hex; the protocol also allows unpadded urlsafe base64.
		if !strings.EqualFold(v, hex.EncodeToString(file.md5)) && strings.TrimRight(v, "=") != base64.RawURLEncoding.EncodeToString(file.md5) {
			return fmt.Errorf("md5_digest does not match the uploaded file: %w", core.ErrDigestMismatch)
		}
	}
	if v := strings.ToLower(first(form, "blake2_256_digest")); v != "" {
		declared = true
		if v != hex.EncodeToString(file.blake2) {
			return fmt.Errorf("blake2_256_digest does not match the uploaded file: %w", core.ErrDigestMismatch)
		}
	}
	if !declared {
		return fmt.Errorf("one of sha256_digest, md5_digest or blake2_256_digest is required: %w", core.ErrInvalid)
	}
	return nil
}

// metadataAttributes keeps the core metadata fields of the form for the UI and search.
func metadataAttributes(form map[string][]string) map[string]any {
	out := map[string]any{}
	for k, vs := range form {
		if protocolFields[k] || len(k) > 64 {
			continue
		}
		var kept []string
		for _, v := range vs {
			if v = strings.TrimSpace(v); v != "" && v != "UNKNOWN" {
				if k == "description" && len(v) > maxDescriptionLen {
					v = v[:maxDescriptionLen]
				}
				kept = append(kept, v)
			}
		}
		switch {
		case len(kept) == 0:
		case multiValued[k] || len(kept) > 1:
			out[k] = kept
		default:
			out[k] = kept[0]
		}
	}
	return out
}

type wheelMeta struct {
	body   []byte
	digest core.Digest
	size   int64
}

// extractWheelMetadata copies {name}-{version}.dist-info/METADATA out of the
// stored wheel into its own blob (PEP 658). The wheel is read through the
// blob store with random access; it is never loaded into memory.
func (h *handler) extractWheelMetadata(ctx context.Context, bs core.BlobStore, file *stored, project string) (*wheelMeta, error) {
	rc, err := bs.Get(ctx, file.digest)
	if err != nil {
		return nil, fmt.Errorf("open stored wheel: %w", err)
	}
	defer rc.Close()
	zr, err := zip.NewReader(&seekReaderAt{rs: rc}, file.size)
	if err != nil {
		return nil, fmt.Errorf("wheel %s is not a zip archive: %w", file.filename, core.ErrInvalid)
	}
	var entry *zip.File
	for _, f := range zr.File {
		dir, base, ok := strings.Cut(f.Name, "/")
		if !ok || base != "METADATA" || !strings.HasSuffix(dir, ".dist-info") {
			continue
		}
		distName, _, _ := strings.Cut(strings.TrimSuffix(dir, ".dist-info"), "-")
		if Normalize(distName) == project {
			entry = f
			break
		}
	}
	if entry == nil {
		return nil, fmt.Errorf("wheel %s has no %s .dist-info/METADATA: %w", file.filename, project, core.ErrInvalid)
	}
	if entry.UncompressedSize64 > maxMetadataBytes {
		return nil, fmt.Errorf("wheel %s METADATA is too large: %w", file.filename, core.ErrInvalid)
	}
	f, err := entry.Open()
	if err != nil {
		return nil, fmt.Errorf("wheel %s METADATA: %w: %w", file.filename, err, core.ErrInvalid)
	}
	body, err := io.ReadAll(io.LimitReader(f, maxMetadataBytes+1))
	_ = f.Close()
	if err != nil || len(body) > maxMetadataBytes {
		return nil, fmt.Errorf("wheel %s METADATA unreadable: %w", file.filename, core.ErrInvalid)
	}
	d, n, err := bs.Put(ctx, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("store metadata: %w", err)
	}
	return &wheelMeta{body: body, digest: d, size: n}, nil
}

// metadataHeader returns the first value of an RFC 822 style header in a
// core metadata document.
func metadataHeader(doc []byte, name string) string {
	sc := bufio.NewScanner(bytes.NewReader(doc))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break // end of headers; the body is the description
		}
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// seekReaderAt adapts a blob reader to io.ReaderAt for archive/zip.
type seekReaderAt struct {
	mu sync.Mutex
	rs io.ReadSeeker
}

func (s *seekReaderAt) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.rs.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(s.rs, p)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return n, err
}

func first(form map[string][]string, k string) string {
	if vs := form[k]; len(vs) > 0 {
		return strings.TrimSpace(vs[0])
	}
	return ""
}

func contentType(filename string) string {
	l := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(l, ".whl"), strings.HasSuffix(l, ".zip"), strings.HasSuffix(l, ".egg"):
		return "application/zip"
	case strings.HasSuffix(l, ".tar.gz"), strings.HasSuffix(l, ".tgz"):
		return "application/gzip"
	case strings.HasSuffix(l, metadataSuffix):
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}
