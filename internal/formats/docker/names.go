package docker

import (
	"regexp"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// routeKind is the OCI endpoint a /v2/ path addresses.
type routeKind int

const (
	routeInvalid routeKind = iota
	routePing
	routeToken
	routeCatalog
	routeTags
	routeManifest
	routeBlob
	routeUploadStart
	routeUpload
	routeReferrers
)

// route is a parsed /v2/ request path. Name is the OCI repository name the
// client used: "<hangar repository>/<image path>", e.g. "docker-hosted/team/app".
type route struct {
	kind  routeKind
	name  string
	repo  string // Hangar repository name (first segment of name)
	image string // image path inside the Hangar repository
	ref   string // tag, digest or upload id
}

// nameRE is the OCI repository name grammar (distribution-spec "Pulling manifests").
var (
	nameRE = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)
	tagRE  = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
)

const maxNameLength = 255

// parseRoute parses the path after "/v2/".
func parseRoute(rest string) route {
	rest = strings.TrimPrefix(rest, "/")
	switch rest {
	case "":
		return route{kind: routePing}
	case "token":
		return route{kind: routeToken}
	case "_catalog":
		return route{kind: routeCatalog}
	}
	if name, ok := strings.CutSuffix(rest, "/tags/list"); ok {
		return withName(route{kind: routeTags}, name)
	}
	trimmed := strings.TrimSuffix(rest, "/")
	seg := strings.Split(trimmed, "/")
	n := len(seg)
	switch {
	case n >= 3 && seg[n-2] == "blobs" && seg[n-1] == "uploads":
		return withName(route{kind: routeUploadStart}, strings.Join(seg[:n-2], "/"))
	case n >= 4 && seg[n-3] == "blobs" && seg[n-2] == "uploads":
		return withName(route{kind: routeUpload, ref: seg[n-1]}, strings.Join(seg[:n-3], "/"))
	case trimmed != rest:
		// Only the upload-start endpoint has a trailing slash.
		return route{}
	case n >= 3 && seg[n-2] == "manifests":
		return withName(route{kind: routeManifest, ref: seg[n-1]}, strings.Join(seg[:n-2], "/"))
	case n >= 3 && seg[n-2] == "blobs":
		return withName(route{kind: routeBlob, ref: seg[n-1]}, strings.Join(seg[:n-2], "/"))
	case n >= 3 && seg[n-2] == "referrers":
		return withName(route{kind: routeReferrers, ref: seg[n-1]}, strings.Join(seg[:n-2], "/"))
	}
	return route{}
}

// withName fills name, repo and image. An invalid name yields routeInvalid
// with name set so the handler can answer NAME_INVALID.
func withName(rt route, name string) route {
	rt.name = name
	repo, image, ok := splitName(name)
	if !ok {
		rt.kind = routeInvalid
		return rt
	}
	rt.repo, rt.image = repo, image
	return rt
}

// splitName splits an OCI name into the Hangar repository and the image path.
// Both parts are required: images live inside a Hangar repository.
func splitName(name string) (repo, image string, ok bool) {
	if len(name) > maxNameLength || !nameRE.MatchString(name) {
		return "", "", false
	}
	repo, image, ok = strings.Cut(name, "/")
	if !ok || image == "" || core.ValidateRepositoryName(repo) != nil {
		return "", "", false
	}
	return repo, image, true
}

func validTag(s string) bool { return tagRE.MatchString(s) }

// isDigest reports whether ref looks like a digest (tags cannot contain ':').
func isDigest(ref string) bool { return strings.Contains(ref, ":") }

// parseOCIDigest validates a digest from a client. Only sha256 and sha512 are
// accepted (the algorithms registries are required/allowed to support).
func parseOCIDigest(s string) (core.Digest, bool) {
	d, err := core.ParseDigest(s)
	return d, err == nil
}

// Asset layout inside a Hangar repository. Paths mirror the URLs clients use:
//
//	<image>/blobs/<digest>                      layer or config blob
//	<image>/manifests/<digest>                  manifest by digest (no component)
//	<image>/manifests/<tag>                     tag -> manifest (component image:tag)
//	<image>/referrers/<subject>/<digest>        referrers index entry (metadata)
//
// Image path components cannot contain ':' and tags cannot either, so a
// digest and a tag never collide; nested images are filtered out of listings
// because their remainder contains '/'.
func blobPath(image string, d core.Digest) string { return image + "/blobs/" + string(d) }

func manifestPath(image, ref string) string { return image + "/manifests/" + ref }

func manifestPrefix(image string) string { return image + "/manifests/" }

func referrerPrefix(image string, subject core.Digest) string {
	return image + "/referrers/" + string(subject) + "/"
}

func referrerPath(image string, subject, d core.Digest) string {
	return referrerPrefix(image, subject) + string(d)
}

// splitImage returns the component namespace and name for an image path:
// "team/app" -> ("team", "app"), "app" -> ("", "app").
func splitImage(image string) (namespace, name string) {
	if i := strings.LastIndexByte(image, '/'); i >= 0 {
		return image[:i], image[i+1:]
	}
	return "", image
}

func joinImage(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}

// parseAssetPath is the inverse of the layout above: it returns the image,
// the kind ("blob", "manifest", "tag", "referrer") and the last segment.
func parseAssetPath(p string) (image, kind, ref string, ok bool) {
	p = core.CleanPath(p)
	seg := strings.Split(p, "/")
	n := len(seg)
	if n >= 4 && seg[n-3] == "referrers" && isDigest(seg[n-2]) {
		return strings.Join(seg[:n-3], "/"), "referrer", seg[n-1], true
	}
	if n < 3 {
		return "", "", "", false
	}
	image, ref = strings.Join(seg[:n-2], "/"), seg[n-1]
	switch seg[n-2] {
	case "blobs":
		return image, "blob", ref, true
	case "manifests":
		if isDigest(ref) {
			return image, "manifest", ref, true
		}
		return image, "tag", ref, true
	}
	return "", "", "", false
}
