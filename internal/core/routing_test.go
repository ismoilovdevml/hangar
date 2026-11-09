package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRoutingName(t *testing.T) {
	for _, c := range []struct{ format, in, want string }{
		{"nuget", "Edcom.Common.Utils", "edcom.common.utils"},
		{"pypi", "Edcom__Utils.Core", "edcom-utils-core"},
		{"pypi", "_private.", "-private-"},
		{"cargo", "Edcom_Utils", "edcom-utils"},
		{"npm", "@trk/UI", "@trk/UI"},
		{"raw", "/a/b.txt", "a/b.txt"},
		{"maven", "com.acme:lib", "com.acme:lib"},
	} {
		if got := RoutingName(c.format, c.in); got != c.want {
			t.Errorf("RoutingName(%s, %q) = %q, want %q", c.format, c.in, got, c.want)
		}
	}
}

func TestMatchRoutingPattern(t *testing.T) {
	for _, c := range []struct {
		format, pattern, name string
		want                  bool
	}{
		{"nuget", "Edcom.*", "edcom.common.utils", true},
		{"nuget", "edcom.*", "Edcom.Common.Utils", true},
		{"nuget", "Edcom.*", "Edcomx", false},
		{"nuget", "Edcom.?ata", "Edcom.Data", true},
		{"pypi", "edcom-*", "Edcom_Utils", true},
		{"pypi", "edcom_utils", "edcom.utils", true},
		{"cargo", "edcom-*", "edcom_core", true},
		{"npm", "@trk/*", "@trk/ui", true},
		{"npm", "@trk/*", "@trkx/ui", false},
		{"npm", "@trk/*", "@TRK/ui", false},
		{"maven", "com.edcom.*", "com.edcom.common:utils", true},
		{"maven", "com.edcom", "com.edcom:utils", true},
		{"maven", "com.edcom", "com.edcomx:utils", false},
		{"maven", "com.edcom:util*", "com.edcom:utils", true},
		{"maven", "com.edcom:util*", "com.edcom:core", false},
		{"go", "git.example.com/corp", "git.example.com/corp/lib", true},
		{"go", "git.example.com/corp", "git.example.com/corp", true},
		{"go", "git.example.com/corp", "git.example.com/corporate/lib", false},
		{"go", "*.example.com", "git.example.com/corp/lib", true},
		{"go", "git.example.com/corp/", "git.example.com/corp/lib", true},
		{"go", "git.example.com/corp/lib", "git.example.com/corp", false},
		{"docker", "team/*", "team/app/sub", true},
		{"docker", "library/nginx", "library/nginx", true},
		{"helm", "edcom-*", "edcom-api", true},
		{"raw", "internal/*", "/internal/a/b.txt", true},
		{"apt", "edcom-*", "edcom-tools", true},
		{"rpm", "edcom-*", "other", false},
		{"nuget", "*", "anything", true},
	} {
		if got := MatchRoutingPattern(c.format, c.pattern, c.name); got != c.want {
			t.Errorf("Match(%s, %q, %q) = %v, want %v", c.format, c.pattern, c.name, got, c.want)
		}
	}
}

func TestGlob(t *testing.T) {
	for _, c := range []struct {
		p, s string
		want bool
	}{
		{"", "", true}, {"", "a", false}, {"*", "", true}, {"a*b*c", "aXbYc", true},
		{"a*b*c", "aXbY", false}, {"**", "x/y", true}, {"a?c", "abc", true}, {"a?c", "ac", false},
		{"*.utils", "edcom.common.utils", true}, {"é*", "éa", true},
	} {
		if got := glob(c.p, c.s); got != c.want {
			t.Errorf("glob(%q, %q) = %v, want %v", c.p, c.s, got, c.want)
		}
	}
}

func routingGroup(rules *RoutingRules) *Repository {
	return &Repository{Name: "nuget-group", Format: "nuget", Type: RepoGroup,
		Config: RepoConfig{Group: &GroupConfig{Members: []string{"nuget-hosted", "nuget-proxy", "nuget-other"}}, Routing: rules}}
}

func TestMemberRoutingBlock(t *testing.T) {
	g := routingGroup(&RoutingRules{Members: []MemberRouting{
		{Member: "nuget-hosted", Exclusive: []string{"Edcom.*"}},
		{Member: "nuget-other", Block: []string{"Bad.*"}},
	}})
	if b := g.MemberRoutingBlock("nuget-hosted", "edcom.common.utils"); b != nil {
		t.Fatalf("owner blocked: %v", b)
	}
	b := g.MemberRoutingBlock("nuget-proxy", "Edcom.Common.Utils")
	if b == nil || b.Rule != RoutingRuleExclusive || b.Pattern != "Edcom.*" || b.Member != "nuget-proxy" ||
		len(b.Owners) != 1 || b.Owners[0] != "nuget-hosted" {
		t.Fatalf("exclusive block = %+v", b)
	}
	if !errors.Is(b, ErrNotFound) || !errors.Is(b, ErrRoutingBlocked) {
		t.Fatal("RoutingBlock must wrap ErrNotFound and ErrRoutingBlocked")
	}
	if got := b.Error(); !strings.Contains(got, `"Edcom.*"`) || !strings.Contains(got, `"nuget-group"`) || !strings.Contains(got, `reserved for "nuget-hosted"`) {
		t.Fatalf("reason = %q", got)
	}
	if b := g.MemberRoutingBlock("nuget-proxy", "Newtonsoft.Json"); b != nil {
		t.Fatalf("unclaimed name blocked: %v", b)
	}
	if b := g.MemberRoutingBlock("nuget-other", "bad.lib"); b == nil || b.Rule != RoutingRuleBlock {
		t.Fatalf("member block = %+v", b)
	}
	if b := g.MemberRoutingBlock("nuget-hosted", "bad.lib"); b != nil {
		t.Fatalf("block leaked to another member: %v", b)
	}
	// Two owners share a name: both serve it, the rest do not.
	g.Config.Routing.Members = append(g.Config.Routing.Members, MemberRouting{Member: "nuget-proxy", Exclusive: []string{"Edcom.Shared"}})
	if b := g.MemberRoutingBlock("nuget-proxy", "edcom.shared"); b != nil {
		t.Fatalf("second owner blocked: %v", b)
	}
	if b := g.MemberRoutingBlock("nuget-other", "edcom.shared"); b == nil || len(b.Owners) != 2 {
		t.Fatalf("non-owner = %+v", b)
	}
	if b := (&Repository{Type: RepoGroup}).MemberRoutingBlock("x", "y"); b != nil {
		t.Fatal("no rules must allow")
	}
}

func TestProxyRoutingBlock(t *testing.T) {
	p := &Repository{Name: "npm-proxy", Format: "npm", Type: RepoProxy, BlobStore: "default",
		Config: RepoConfig{Proxy: &ProxyConfig{RemoteURL: "https://registry.example.org"},
			Routing: &RoutingRules{Block: []string{"@trk/*"}}}}
	b := p.RoutingBlock("@trk/ui")
	if b == nil || b.Rule != RoutingRuleBlock || b.Repository != "npm-proxy" || b.Member != "" {
		t.Fatalf("block = %+v", b)
	}
	if want := `blocked by routing rule "@trk/*" on repository "npm-proxy"`; b.Error() != want {
		t.Fatalf("reason = %q, want %q", b.Error(), want)
	}
	if p.RoutingBlock("lodash") != nil {
		t.Fatal("lodash blocked")
	}
	p.Config.Routing.Allow = []string{"@vendor/*"}
	if b := p.RoutingBlock("lodash"); b == nil || b.Rule != RoutingRuleAllow {
		t.Fatalf("allow miss = %+v", b)
	}
	if p.RoutingBlock("@vendor/sdk") != nil {
		t.Fatal("allowed name blocked")
	}
	p.Config.Routing.Allow = []string{"@trk/*"}
	if b := p.RoutingBlock("@trk/ui"); b == nil || b.Rule != RoutingRuleBlock {
		t.Fatal("block must win over allow")
	}
	hosted := &Repository{Name: "h", Type: RepoHosted, Config: RepoConfig{Routing: &RoutingRules{Block: []string{"x"}}}}
	if hosted.RoutingBlock("x") != nil {
		t.Fatal("hosted repositories have no own rules")
	}
}

func TestValidateRouting(t *testing.T) {
	proxy := func(rt *RoutingRules) *Repository {
		return &Repository{Name: "go-proxy", Format: "go", Type: RepoProxy, BlobStore: "default",
			Config: RepoConfig{Proxy: &ProxyConfig{RemoteURL: "https://proxy.example.org"}, Routing: rt}}
	}
	for _, c := range []struct {
		name string
		r    *Repository
		ok   bool
	}{
		{"proxy ok", proxy(&RoutingRules{Block: []string{"git.example.com/corp"}}), true},
		{"proxy members", proxy(&RoutingRules{Members: []MemberRouting{{Member: "x"}}}), false},
		{"bad go glob", proxy(&RoutingRules{Block: []string{"git.example.com/["}}), false},
		{"space", proxy(&RoutingRules{Block: []string{"a b"}}), false},
		{"empty", proxy(&RoutingRules{Allow: []string{""}}), false},
		{"dup", proxy(&RoutingRules{Block: []string{"a", "a"}}), false},
		{"long", proxy(&RoutingRules{Block: []string{strings.Repeat("a", MaxRoutingPatternLn+1)}}), false},
		{"group ok", routingGroup(&RoutingRules{Members: []MemberRouting{{Member: "nuget-hosted", Exclusive: []string{"Edcom.*"}}}}), true},
		{"group block", routingGroup(&RoutingRules{Block: []string{"x"}}), false},
		{"non-member", routingGroup(&RoutingRules{Members: []MemberRouting{{Member: "gone", Block: []string{"x"}}}}), false},
		{"member twice", routingGroup(&RoutingRules{Members: []MemberRouting{{Member: "nuget-hosted"}, {Member: "nuget-hosted"}}}), false},
		{"hosted", &Repository{Name: "h1", Format: "npm", Type: RepoHosted, BlobStore: "d", Config: RepoConfig{Routing: &RoutingRules{Block: []string{"x"}}}}, false},
		{"hosted empty", &Repository{Name: "h1", Format: "npm", Type: RepoHosted, BlobStore: "d", Config: RepoConfig{Routing: &RoutingRules{}}}, true},
		{"maven colons", &Repository{Name: "m1", Format: "maven", Type: RepoProxy, BlobStore: "d",
			Config: RepoConfig{Proxy: &ProxyConfig{RemoteURL: "https://repo.example.org"}, Routing: &RoutingRules{Block: []string{"a:b:c"}}}}, false},
	} {
		err := c.r.Validate()
		if (err == nil) != c.ok {
			t.Errorf("%s: Validate = %v, want ok=%v", c.name, err, c.ok)
		}
		if err != nil && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: error %v does not wrap ErrInvalid", c.name, err)
		}
	}
}

func TestRoutingRulesJSONAndClone(t *testing.T) {
	in := `{"routing":{"members":[{"member":"a","exclusive":["X.*"]}]}}`
	var c RepoConfig
	if err := json.Unmarshal([]byte(in), &c); err != nil {
		t.Fatal(err)
	}
	if c.Routing.Empty() || c.Routing.Members[0].Exclusive[0] != "X.*" {
		t.Fatalf("decoded %+v", c.Routing)
	}
	cl := c.Routing.Clone()
	cl.Members[0].Exclusive[0] = "Y"
	if c.Routing.Members[0].Exclusive[0] != "X.*" {
		t.Fatal("Clone shares slices")
	}
	if !(*RoutingRules)(nil).Empty() || (*RoutingRules)(nil).Clone() != nil {
		t.Fatal("nil rules")
	}
	if !(&RoutingRules{Members: []MemberRouting{{Member: "a"}}}).Empty() {
		t.Fatal("member without patterns is empty")
	}
	b, _ := json.Marshal(RepoConfig{})
	if strings.Contains(string(b), "routing") {
		t.Fatalf("empty config marshals routing: %s", b)
	}
}
