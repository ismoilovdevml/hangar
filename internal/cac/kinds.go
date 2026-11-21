package cac

import (
	"slices"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/api/labels"
)

// Document kinds.
const (
	KindOutboundProxy = "OutboundProxy"
	KindBlobStore     = "BlobStore"
	KindRole          = "Role"
	KindRepository    = "Repository"
	KindCleanupPolicy = "CleanupPolicy"
)

// Path addresses a value inside a spec: the keys from the spec root.
type Path []string

// String renders the path with dots, e.g. "config.proxy.auth.token".
func (p Path) String() string { return strings.Join(p, ".") }

func (p Path) child(k string) Path { return append(slices.Clip(p), k) }

func (p Path) equal(q Path) bool { return slices.Equal(p, q) }

func paths(ps ...string) []Path {
	out := make([]Path, 0, len(ps))
	for _, p := range ps {
		out = append(out, strings.Split(p, "."))
	}
	return out
}

// kind describes how one document kind maps to the REST API.
type kind struct {
	Name       string // document kind, e.g. "Repository"
	LabelKind  string // {kind} of /resource-labels
	Collection string // collection path under /api/v1
	// stage orders kinds so that what a resource refers to exists first:
	// outbound proxies and blob stores before repositories, member
	// repositories before groups, repositories before cleanup policies.
	// Groups take stage+1 (see stageOf).
	stage int
	// fields are the top-level keys of the input schema other than name;
	// live state is projected onto them.
	fields []string
	// writeOnly are secrets the API accepts but never returns. They cannot
	// be compared, are left out of the diff and exported as references.
	writeOnly []Path
	// urlSecrets are URLs whose password the API returns redacted.
	urlSecrets []Path
	// sensitive are settings the API returns only to administrators that
	// may carry a secret; export writes them as references.
	sensitive []Path
	// atomic objects are replaced as a whole on update and compared
	// exactly, because their shape depends on a discriminator (auth kind).
	atomic []Path
	// unordered lists come back from the API sorted; they compare as sets.
	unordered []Path
	// paged lists are cursor-paginated.
	paged bool
}

var kinds = []*kind{
	{
		Name: KindOutboundProxy, LabelKind: labels.KindProxy, Collection: "/proxies", stage: 0,
		fields:    []string{"scheme", "host", "port", "username", "password", "clear_password", "no_proxy", "redirect_hosts", "ca_pem", "default"},
		writeOnly: paths("password", "clear_password"),
	},
	{
		Name: KindBlobStore, LabelKind: labels.KindBlobStore, Collection: "/blob-stores", stage: 1,
		fields:    []string{"kind", "soft_quota_bytes", "fs", "s3"},
		writeOnly: paths("s3.secret_access_key"),
	},
	{
		Name: KindRole, LabelKind: labels.KindRole, Collection: "/roles", stage: 2,
		fields:    []string{"description", "privileges", "group_mappings"},
		unordered: paths("privileges", "group_mappings"),
	},
	{
		Name: KindRepository, LabelKind: labels.KindRepository, Collection: "/repositories", stage: 3,
		fields:     []string{"format", "type", "description", "blob_store", "online", "config"},
		writeOnly:  paths("config.proxy.auth.password", "config.proxy.auth.token"),
		urlSecrets: paths("config.proxy.remote_url", "config.proxy.http.proxy"),
		sensitive:  paths("config.format.service_index"),
		atomic:     paths("config.proxy.auth"),
		paged:      true,
	},
	{
		Name: KindCleanupPolicy, LabelKind: labels.KindCleanupPolicy, Collection: "/cleanup-policies", stage: 5,
		fields:    []string{"description", "format", "criteria", "repositories", "schedule", "enabled"},
		unordered: paths("repositories"),
		paged:     true,
	},
}

// KindNames lists the document kinds in apply order.
func KindNames() []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, k.Name)
	}
	return out
}

func kindByName(name string) *kind {
	for _, k := range kinds {
		if k.Name == name {
			return k
		}
	}
	return nil
}

func (k *kind) itemPath(name string) string { return k.Collection + "/" + escapePath(name) }

func (k *kind) isWriteOnly(p Path) bool { return containsPath(k.writeOnly, p) }

func (k *kind) isAtomic(p Path) bool { return containsPath(k.atomic, p) }

func (k *kind) isUnordered(p Path) bool { return containsPath(k.unordered, p) }

func (k *kind) isURLSecret(p Path) bool { return containsPath(k.urlSecrets, p) }

func containsPath(ps []Path, p Path) bool {
	return slices.ContainsFunc(ps, p.equal)
}
