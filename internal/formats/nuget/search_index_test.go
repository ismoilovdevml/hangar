package nuget_test

import "testing"

// Text search matches word prefixes (the full-text index rule), not
// arbitrary substrings.
func TestSearchMatchesWordPrefixes(t *testing.T) {
	f := newFixture(t)
	f.mustPush("nuget-hosted", "Acme.Json", "1.0.0", "<title>Quantum widgets</title>")
	f.mustPush("nuget-hosted", "Other.Lib", "1.0.0", "")
	for query, want := range map[string]string{
		"q=acm":          "Acme.Json@1.0.0",
		"q=ACME.JS":      "Acme.Json@1.0.0",
		"q=quant":        "Acme.Json@1.0.0",
		"q=uant":         "",
		"q=cme":          "",
		"q=widg+other":   "",
		"q=other.lib":    "Other.Lib@1.0.0",
		"q=title:widget": "Acme.Json@1.0.0",
	} {
		var res searchResult
		f.getJSON("/repository/nuget-hosted/query?"+query, &res)
		if got := res.ids(); got != want {
			t.Errorf("%s = %q, want %q", query, got, want)
		}
	}
}
