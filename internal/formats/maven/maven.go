// Package maven implements the Maven repository format: the Maven 2 layout
// (https://maven.apache.org/repository/layout.html) served under
// /repository/{repo}/, with maven-metadata.xml generation
// (https://maven.apache.org/ref/3.9.6/maven-repository-metadata/repository-metadata.html),
// checksum side files, signatures, release/snapshot version policies, proxying
// of remote Maven repositories and group merging.
package maven

import (
	"fmt"
	"slices"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Name is the format identifier stored in repositories.format.
const Name = "maven"

// Version policies (Config.VersionPolicy).
const (
	// PolicyRelease accepts only release versions (no -SNAPSHOT).
	PolicyRelease = "release"
	// PolicySnapshot accepts only -SNAPSHOT versions.
	PolicySnapshot = "snapshot"
	// PolicyMixed accepts both. The default.
	PolicyMixed = "mixed"
)

// Layout policies (Config.LayoutPolicy).
const (
	// LayoutStrict rejects uploads whose path is not a Maven artifact,
	// maven-metadata.xml or checksum. The default.
	LayoutStrict = "strict"
	// LayoutPermissive also stores other files (archetype-catalog.xml, ...).
	LayoutPermissive = "permissive"
)

// Config is the format-specific repository configuration (RepoConfig.Format):
//
//	format:
//	  version_policy: release   # release | snapshot | mixed (default)
//	  layout_policy: strict     # strict (default) | permissive
type Config struct {
	VersionPolicy string `json:"version_policy,omitempty"`
	LayoutPolicy  string `json:"layout_policy,omitempty"`
}

var configKeys = []string{"version_policy", "layout_policy"}

// ConfigOf returns repo's Maven settings with defaults applied.
func ConfigOf(repo *core.Repository) (Config, error) {
	var c Config
	for k := range repo.Config.Format {
		if !slices.Contains(configKeys, k) {
			return c, fmt.Errorf("repository %q: unknown maven setting %q (known: %v): %w", repo.Name, k, configKeys, core.ErrInvalid)
		}
	}
	if err := repo.Config.DecodeFormat(&c); err != nil {
		return c, fmt.Errorf("repository %q: %w", repo.Name, err)
	}
	switch c.VersionPolicy {
	case "":
		c.VersionPolicy = PolicyMixed
	case PolicyRelease, PolicySnapshot, PolicyMixed:
	default:
		return c, fmt.Errorf("repository %q: version_policy %q: want release, snapshot or mixed: %w", repo.Name, c.VersionPolicy, core.ErrInvalid)
	}
	switch c.LayoutPolicy {
	case "":
		c.LayoutPolicy = LayoutStrict
	case LayoutStrict, LayoutPermissive:
	default:
		return c, fmt.Errorf("repository %q: layout_policy %q: want strict or permissive: %w", repo.Name, c.LayoutPolicy, core.ErrInvalid)
	}
	return c, nil
}

// allowsVersion reports whether the version policy admits a (base) version.
func (c Config) allowsVersion(v string) bool {
	switch c.VersionPolicy {
	case PolicyRelease:
		return !isSnapshot(v)
	case PolicySnapshot:
		return isSnapshot(v)
	}
	return true
}

// Format implements core.Format for Maven.
type Format struct{}

var _ core.Format = (*Format)(nil)

// New returns the Maven format.
func New() *Format { return &Format{} }

// Name implements core.Format.
func (f *Format) Name() string { return Name }

// Mount implements core.Format. Routes are relative to /repository/{repo}/.
func (f *Format) Mount(r chi.Router, deps core.Deps) {
	h := newHandler(deps)
	r.Get("/*", h.get)
	r.Head("/*", h.get)
	r.Put("/*", h.put)
	r.Delete("/*", h.delete)
}

// Validate implements core.Format.
func (f *Format) Validate(repo *core.Repository) error {
	if repo.Format != Name {
		return fmt.Errorf("repository %q has format %q, not %s: %w", repo.Name, repo.Format, Name, core.ErrInvalid)
	}
	if _, err := ConfigOf(repo); err != nil {
		return err
	}
	if repo.Type == core.RepoGroup && repo.Config.Group != nil && repo.Config.Group.WriteMember != "" {
		return fmt.Errorf("repository %q: maven groups are read-only; deploy to the hosted repository: %w", repo.Name, core.ErrInvalid)
	}
	return nil
}

// Coordinates implements core.Format: groupId:artifactId:version, with
// classifier, extension and the timestamped snapshot version in Extra.
func (f *Format) Coordinates(a *core.Asset) core.Coordinates {
	mp, err := parsePath(a.Path)
	if err != nil || mp.kind != kindArtifact {
		return core.Coordinates{Format: Name, Name: a.Path}
	}
	c := core.Coordinates{Format: Name, Namespace: mp.groupID, Name: mp.artifactID, Version: mp.version,
		Extra: map[string]string{"extension": mp.extension}}
	if mp.classifier != "" {
		c.Extra["classifier"] = mp.classifier
	}
	if mp.fileVersion != mp.version {
		c.Extra["snapshot_version"] = mp.fileVersion
	}
	return c
}
