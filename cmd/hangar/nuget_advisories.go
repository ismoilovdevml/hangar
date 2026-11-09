package main

import (
	"context"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/nuget"
	"github.com/ismoilovdevml/hangar/internal/scanning"
)

// nugetAdvisories serves the open scan findings of hosted NuGet
// repositories to the NuGet VulnerabilityInfo resource (NuGet Audit).
type nugetAdvisories struct{ scanner *scanning.Service }

// maxNuGetAdvisories bounds the findings read for one repository page.
const maxNuGetAdvisories = 50000

func (a nugetAdvisories) Advisories(ctx context.Context, repoID string) ([]nuget.Advisory, time.Time, error) {
	q := scanning.FindingQuery{RepoIDs: []string{repoID}, Statuses: []string{scanning.FindingOpen, scanning.FindingAssigned},
		Limit: core.MaxPageSize}
	var (
		out     []nuget.Advisory
		updated time.Time
	)
	for len(out) < maxNuGetAdvisories {
		page, err := a.scanner.Findings(ctx, q)
		if err != nil {
			return nil, time.Time{}, err
		}
		for _, f := range page.Items {
			if f.Component.Format != nuget.Name || f.Kind != "vulnerability" {
				continue
			}
			out = append(out, nuget.Advisory{ID: f.Component.Name, Version: f.Component.Version,
				Severity: nugetSeverity(f.Severity), URL: advisoryURL(f)})
			for _, t := range []time.Time{f.FirstSeenAt, f.LastSeenAt} {
				if t.After(updated) {
					updated = t
				}
			}
		}
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	if updated.IsZero() {
		updated = time.Unix(0, 0)
	}
	return out, updated, nil
}

// nugetSeverity maps a finding severity to the resource's 0 (low) .. 3
// (critical). Unknown severities count as moderate, so NuGet Audit's
// default level still reports them.
func nugetSeverity(s string) int {
	switch strings.ToLower(s) {
	case "critical":
		return nuget.SeverityCritical
	case "high":
		return nuget.SeverityHigh
	case "low":
		return nuget.SeverityLow
	}
	return nuget.SeverityModerate
}

// advisoryURL is the advisory page of a finding: its ADVISORY reference,
// else the OSV page of its id.
func advisoryURL(f *scanning.Finding) string {
	for _, r := range f.References {
		if strings.EqualFold(r.Type, "ADVISORY") && strings.HasPrefix(r.URL, "https://") {
			return r.URL
		}
	}
	return "https://osv.dev/vulnerability/" + f.AdvisoryID
}
