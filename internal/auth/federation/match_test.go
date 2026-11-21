package federation_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/auth/federation"
)

func TestConditionMatches(t *testing.T) {
	claims := map[string]any{}
	if err := json.Unmarshal([]byte(`{
		"repository": "acme/app", "ref": "refs/heads/release/1.2", "ref_protected": "true",
		"workflow_ref": "acme/app/.github/workflows/release.yml@refs/heads/main",
		"run_attempt": 3, "is_fork": false, "groups": ["a", "b"], "ext": {"team": "platform"},
		"literal": "a*b"
	}`), &claims); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		cond federation.Condition
		want bool
	}{
		{"exact match", federation.Condition{Claim: "repository", Values: []string{"acme/app"}}, true},
		{"exact is the default", federation.Condition{Claim: "repository", Match: "", Values: []string{"acme/app"}}, true},
		{"exact mismatch", federation.Condition{Claim: "repository", Values: []string{"acme/ap"}}, false},
		{"exact is case sensitive", federation.Condition{Claim: "repository", Values: []string{"ACME/app"}}, false},
		{"any value matches", federation.Condition{Claim: "repository", Values: []string{"x/y", "acme/app"}}, true},
		{"exact does not glob", federation.Condition{Claim: "repository", Values: []string{"acme/*"}}, false},
		{"glob star", federation.Condition{Claim: "repository", Match: federation.MatchGlob, Values: []string{"acme/*"}}, true},
		{"glob star crosses slashes", federation.Condition{Claim: "ref", Match: federation.MatchGlob, Values: []string{"refs/heads/*"}}, true},
		{"glob inside", federation.Condition{Claim: "workflow_ref", Match: federation.MatchGlob,
			Values: []string{"acme/app/.github/workflows/release.yml@*"}}, true},
		{"glob question mark", federation.Condition{Claim: "ref", Match: federation.MatchGlob, Values: []string{"refs/heads/release/1.?"}}, true},
		{"glob anchored at end", federation.Condition{Claim: "ref", Match: federation.MatchGlob, Values: []string{"refs/heads/release"}}, false},
		{"glob anchored at start", federation.Condition{Claim: "repository", Match: federation.MatchGlob, Values: []string{"cme/*"}}, false},
		{"glob star matches literal star", federation.Condition{Claim: "literal", Match: federation.MatchGlob, Values: []string{"a*"}}, true},
		{"string boolean", federation.Condition{Claim: "ref_protected", Values: []string{"true"}}, true},
		{"json boolean", federation.Condition{Claim: "is_fork", Values: []string{"false"}}, true},
		{"number", federation.Condition{Claim: "run_attempt", Values: []string{"3"}}, true},
		{"dotted path", federation.Condition{Claim: "ext.team", Values: []string{"platform"}}, true},
		{"missing claim", federation.Condition{Claim: "environment", Match: federation.MatchGlob, Values: []string{"p*"}}, false},
		{"array never matches", federation.Condition{Claim: "groups", Values: []string{"a"}}, false},
		{"object never matches", federation.Condition{Claim: "ext", Match: federation.MatchGlob, Values: []string{"?*"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cond.Matches(claims); got != tc.want {
				t.Errorf("Matches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateConditions(t *testing.T) {
	const github = "https://token.actions.githubusercontent.com"
	cond := func(claim, match string, values ...string) federation.Condition {
		return federation.Condition{Claim: claim, Match: match, Values: values}
	}
	cases := []struct {
		name    string
		issuer  string
		conds   []federation.Condition
		wantErr string // substring; "" for success
	}{
		{"no conditions", "https://ci.example.com", nil, "at least one claim condition"},
		{"generic issuer, one condition", "https://ci.example.com", []federation.Condition{cond("project", "", "x")}, ""},
		{"unknown match", "https://ci.example.com", []federation.Condition{cond("project", "regex", "x")}, "want exact or glob"},
		{"bad claim name", "https://ci.example.com", []federation.Condition{cond("a b", "", "x")}, "want a claim name"},
		{"no values", "https://ci.example.com", []federation.Condition{cond("project", "")}, "at least one value"},
		{"empty value", "https://ci.example.com", []federation.Condition{cond("project", "", "")}, "values must be"},
		{"glob matching anything", "https://ci.example.com", []federation.Condition{cond("project", "glob", "**")}, "matches anything"},
		{"github without owner condition", github, []federation.Condition{cond("ref", "", "refs/heads/main")}, "every account"},
		{"github owner glob without slash", github, []federation.Condition{cond("repository", "glob", "acme*")}, "every account"},
		{"github owner glob under owner", github, []federation.Condition{cond("repository", "glob", "acme/*")}, ""},
		{"github exact repository", github, []federation.Condition{cond("repository", "", "acme/app"), cond("ref", "", "refs/heads/main")}, ""},
		{"github enterprise issuer", github + "/acme", []federation.Condition{cond("environment", "", "prod")}, "every account"},
		{"github sub", github, []federation.Condition{cond("sub", "glob", "repo:acme/app:*")}, ""},
		{"gitlab.com without owner", "https://gitlab.com", []federation.Condition{cond("ref_protected", "", "true")}, "every account"},
		{"gitlab.com project path", "https://gitlab.com", []federation.Condition{cond("project_path", "", "acme/app")}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := federation.ValidateConditions(tc.issuer, tc.conds)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateConditionsNormalizes(t *testing.T) {
	out, err := federation.ValidateConditions("https://ci.example.com",
		[]federation.Condition{{Claim: " project ", Match: " GLOB ", Values: []string{"a/*", "a/*", "b/*"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := out[0]; got.Claim != "project" || got.Match != federation.MatchGlob || len(got.Values) != 2 {
		t.Fatalf("normalized = %+v", got)
	}
}
