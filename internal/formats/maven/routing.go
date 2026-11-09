package maven

import (
	"context"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// Routing identity of a Maven path is "groupId:artifactId". Some paths are
// ambiguous: <dirs>/maven-metadata.xml is artifact metadata of
// groupId:artifactId (the last directory being the artifactId) or, for
// plugin groups, group-level metadata of the groupId spelled by every
// directory. routingIdentity returns the artifact reading as primary and the
// group reading as alt. The primary name gets every rule (allow lists
// included); alt only block and exclusive rules, so an allow list written as
// groupId:artifactId patterns does not reject artifact metadata, while a
// blocked groupId keeps its plugin-group metadata away from a remote too.
// Paths outside the layout (archetype-catalog.xml, .index/) have neither and
// are index requests.
func routingIdentity(mp *mavenPath) (primary, alt string) {
	if mp.kind == kindChecksum {
		return routingIdentity(mp.base)
	}
	segs := strings.Split(mp.path, "/")
	dirs := segs[:len(segs)-1]
	switch mp.kind {
	case kindArtifact:
		return mp.groupID + ":" + mp.artifactID, ""
	case kindMetadata:
		if mp.version != "" {
			return mp.groupID + ":" + mp.artifactID, ""
		}
		if mp.artifactID != "" {
			return mp.groupID + ":" + mp.artifactID, strings.Join(dirs, ".")
		}
		if len(dirs) > 0 {
			return strings.Join(dirs, "."), ""
		}
		return "", ""
	}
	// A file outside the layout below group/artifact/version directories.
	if len(segs) >= 4 {
		n := len(segs)
		return strings.Join(segs[:n-3], ".") + ":" + segs[n-3], strings.Join(dirs, ".")
	}
	if len(dirs) > 0 {
		return "", strings.Join(dirs, ".")
	}
	return "", ""
}

// strict reports whether b is a rule that applies to an alternative reading
// (anything but an allow-list miss).
func strict(b *core.RoutingBlock) bool { return b != nil && b.Rule != core.RoutingRuleAllow }

// checkProxy applies a proxy repository's own rules to mp before its cache
// or remote is consulted.
func checkProxy(ctx context.Context, repo *core.Repository, mp *mavenPath) error {
	primary, alt := routingIdentity(mp)
	if primary != "" {
		if err := routing.Check(ctx, repo, primary); err != nil {
			return err
		}
	} else if alt != "" {
		return routing.Check(ctx, repo, alt)
	}
	if alt != "" {
		if b := repo.RoutingBlock(alt); strict(b) {
			routing.Record(ctx, repo.Format, b)
			return b
		}
	}
	return nil
}

// memberBlock returns the group rule that keeps member from serving mp,
// recorded, or nil.
func memberBlock(ctx context.Context, group, member *core.Repository, mp *mavenPath) *core.RoutingBlock {
	primary, alt := routingIdentity(mp)
	var b *core.RoutingBlock
	switch {
	case primary != "":
		b = group.MemberRoutingBlock(member.Name, primary)
		if b == nil && alt != "" {
			b = group.MemberRoutingBlock(member.Name, alt)
		}
	case alt != "":
		b = group.MemberRoutingBlock(member.Name, alt)
	}
	if b != nil {
		routing.Record(ctx, group.Format, b)
	}
	return b
}

// upstreamPackage is the routing identity sent with a remote request for mp.
func upstreamPackage(mp *mavenPath) (string, bool) {
	primary, alt := routingIdentity(mp)
	if primary == "" {
		primary = alt
	}
	return primary, primary == ""
}

// listingNames are every reading of a directory path ("com/acme/lib/") as
// a groupId or groupId:artifactId, for hiding listing entries: a directory
// is hidden when a block or exclusive rule matches any of them.
func listingNames(dir string) []string {
	segs := strings.Split(strings.Trim(dir, "/"), "/")
	if len(segs) == 1 && segs[0] == "" {
		return nil
	}
	var out []string
	for i := 1; i <= len(segs); i++ {
		out = append(out, strings.Join(segs[:i], "."))
		if i < len(segs) {
			out = append(out, strings.Join(segs[:i], ".")+":"+segs[i])
		}
	}
	return out
}

// listingHidden reports whether a group (member rules) or a proxy (its own
// block rules) hides the entry at path from a directory listing. Listings
// are not package requests: nothing is recorded.
func listingHidden(group, repo *core.Repository, path string) bool {
	for _, n := range listingNames(path) {
		if group != nil && group.MemberRoutingBlock(repo.Name, n) != nil {
			return true
		}
		if strict(repo.RoutingBlock(n)) {
			return true
		}
	}
	return false
}
