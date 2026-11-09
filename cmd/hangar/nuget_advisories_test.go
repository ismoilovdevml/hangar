package main

import (
	"testing"

	"github.com/ismoilovdevml/hangar/internal/formats/nuget"
	"github.com/ismoilovdevml/hangar/internal/scanning"
)

func TestNuGetAdvisoryMapping(t *testing.T) {
	for in, want := range map[string]int{"critical": nuget.SeverityCritical, "HIGH": nuget.SeverityHigh,
		"medium": nuget.SeverityModerate, "low": nuget.SeverityLow, "unknown": nuget.SeverityModerate} {
		if got := nugetSeverity(in); got != want {
			t.Errorf("nugetSeverity(%q) = %d, want %d", in, got, want)
		}
	}
	f := &scanning.Finding{AdvisoryID: "GHSA-aaaa-bbbb-cccc", References: []scanning.Reference{
		{Type: "WEB", URL: "https://example.org/blog"}, {Type: "ADVISORY", URL: "https://github.com/advisories/GHSA-aaaa-bbbb-cccc"}}}
	if got := advisoryURL(f); got != "https://github.com/advisories/GHSA-aaaa-bbbb-cccc" {
		t.Errorf("advisoryURL = %q", got)
	}
	f.References = nil
	if got := advisoryURL(f); got != "https://osv.dev/vulnerability/GHSA-aaaa-bbbb-cccc" {
		t.Errorf("advisoryURL without references = %q", got)
	}
}
