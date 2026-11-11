package api

import (
	"encoding/json"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/rollup"
)

// The Overview requests block reports authentication challenges apart from
// refusals, so a client computing the success rate can leave them out.
func TestRequestsReportChallengesApart(t *testing.T) {
	o := &rollup.Overview{Requests: 27300, ServerErrors: 7, Denied: 95, Challenges: 13526}
	b, err := json.Marshal(requestsFrom(o, o.CoverageStart))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["total"] != 27300.0 || got["denied"] != 95.0 || got["auth_challenges"] != 13526.0 || got["server_errors"] != 7.0 {
		t.Errorf("requests = %s", b)
	}
}
