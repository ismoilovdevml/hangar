package core

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDigest(t *testing.T) {
	d := SHA256Of([]byte("hello"))
	if d != "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("SHA256Of = %s", d)
	}
	if d.Algorithm() != "sha256" || len(d.Hex()) != 64 {
		t.Fatalf("parts: %q %q", d.Algorithm(), d.Hex())
	}
	tests := []struct {
		in string
		ok bool
	}{
		{string(d), true},
		{"sha512:" + strings.Repeat("a", 128), true},
		{"sha256:" + strings.Repeat("A", 64), false},
		{"sha256:" + strings.Repeat("a", 63), false},
		{"md5:" + strings.Repeat("a", 32), false},
		{"sha256", false},
		{"sha256:../../../etc/passwd" + strings.Repeat("a", 39), false},
		{"", false},
	}
	for _, tt := range tests {
		_, err := ParseDigest(tt.in)
		if (err == nil) != tt.ok {
			t.Errorf("ParseDigest(%q) err=%v, want ok=%v", tt.in, err, tt.ok)
		}
		if err != nil && !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseDigest(%q) err not ErrInvalid: %v", tt.in, err)
		}
	}
}

func TestPrivilege(t *testing.T) {
	tests := []struct {
		priv   string
		action Action
		res    Resource
		want   bool
	}{
		{"repo:*:read", ActionRead, RepoResource("npm-hosted"), true},
		{"repo:*:read", ActionWrite, RepoResource("npm-hosted"), false},
		{"repo:npm-*:write", ActionWrite, RepoResource("npm-hosted"), true},
		{"repo:npm-*:write", ActionWrite, RepoResource("maven-hosted"), false},
		{"repo:docker-hosted:admin", ActionDelete, RepoResource("docker-hosted"), true},
		{"repo:*:admin", ActionRead, SystemResource(SystemUsers), false},
		{"system:*", ActionAdmin, SystemResource(SystemUsers), true},
		{"system:users:admin", ActionRead, SystemResource(SystemUsers), true},
		{"system:users:read", ActionWrite, SystemResource(SystemUsers), false},
	}
	for _, tt := range tests {
		p, err := ParsePrivilege(tt.priv)
		if err != nil {
			t.Fatalf("ParsePrivilege(%q): %v", tt.priv, err)
		}
		if got := p.Allows(tt.action, tt.res); got != tt.want {
			t.Errorf("%s.Allows(%s, %s) = %v, want %v", tt.priv, tt.action, tt.res, got, tt.want)
		}
	}
	for _, bad := range []string{"", "repo", "repo:x", "user:x:read", "repo::read", "repo:x:fly", "repo:[:read"} {
		if _, err := ParsePrivilege(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParsePrivilege(%q) = %v, want ErrInvalid", bad, err)
		}
	}
	// Round trip through JSON as strings.
	in := []Privilege{RepoPrivilege("raw-hosted", ActionWrite), MustParsePrivilege("system:*")}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `["repo:raw-hosted:write","system:*:*"]` {
		t.Fatalf("json = %s", b)
	}
	var out []Privilege
	if err := json.Unmarshal(b, &out); err != nil || out[1] != in[1] {
		t.Fatalf("unmarshal: %v %v", out, err)
	}
}

func TestRepositoryValidate(t *testing.T) {
	ok := func(r Repository) Repository { return r }
	tests := []struct {
		name string
		repo Repository
		ok   bool
	}{
		{"hosted", ok(Repository{Name: "npm-hosted", Format: "npm", Type: RepoHosted, BlobStore: "default"}), true},
		{"bad name", Repository{Name: "Npm", Format: "npm", Type: RepoHosted, BlobStore: "default"}, false},
		{"short name", Repository{Name: "n", Format: "npm", Type: RepoHosted, BlobStore: "default"}, false},
		{"no format", Repository{Name: "npm-hosted", Type: RepoHosted, BlobStore: "default"}, false},
		{"bad type", Repository{Name: "npm-hosted", Format: "npm", Type: "mirror", BlobStore: "default"}, false},
		{"no store", Repository{Name: "npm-hosted", Format: "npm", Type: RepoHosted}, false},
		{"proxy no url", Repository{Name: "npm-proxy", Format: "npm", Type: RepoProxy, BlobStore: "default"}, false},
		{"proxy", Repository{Name: "npm-proxy", Format: "npm", Type: RepoProxy, BlobStore: "default",
			Config: RepoConfig{Proxy: &ProxyConfig{RemoteURL: "https://registry.npmjs.org"}}}, true},
		{"group empty", Repository{Name: "npm-all", Format: "npm", Type: RepoGroup}, false},
		{"group", Repository{Name: "npm-all", Format: "npm", Type: RepoGroup,
			Config: RepoConfig{Group: &GroupConfig{Members: []string{"npm-hosted"}}}}, true},
		{"bad policy", Repository{Name: "npm-hosted", Format: "npm", Type: RepoHosted, BlobStore: "default",
			Config: RepoConfig{Hosted: &HostedConfig{WritePolicy: "sometimes"}}}, false},
	}
	for _, tt := range tests {
		err := tt.repo.Validate()
		if (err == nil) != tt.ok {
			t.Errorf("%s: err=%v want ok=%v", tt.name, err, tt.ok)
		}
	}
	r := Repository{Type: RepoHosted}
	if r.WritePolicy() != WriteAllow {
		t.Errorf("default hosted policy = %s", r.WritePolicy())
	}
	r = Repository{Type: RepoProxy}
	if r.WritePolicy() != WriteDeny {
		t.Errorf("proxy policy = %s", r.WritePolicy())
	}
}

func TestRepoConfigJSON(t *testing.T) {
	in := RepoConfig{
		Proxy:  &ProxyConfig{RemoteURL: "https://repo1.maven.org/maven2", MetadataTTL: Duration(24 * time.Hour)},
		Format: map[string]any{"version_policy": "release"},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"metadata_ttl":"24h0m0s"`) {
		t.Fatalf("duration not encoded as string: %s", b)
	}
	var out RepoConfig
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Proxy.MetadataTTL.D() != 24*time.Hour {
		t.Fatalf("ttl = %v", out.Proxy.MetadataTTL.D())
	}
	var mc struct {
		VersionPolicy string `json:"version_policy"`
	}
	if err := out.DecodeFormat(&mc); err != nil || mc.VersionPolicy != "release" {
		t.Fatalf("DecodeFormat = %+v, %v", mc, err)
	}
}

func TestCredentialsFromRequest(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if !CredentialsFromRequest(r).Empty() {
		t.Fatal("expected empty")
	}
	r.SetBasicAuth("alice", "secret")
	if c := CredentialsFromRequest(r); c.Username != "alice" || c.Password != "secret" {
		t.Fatalf("basic = %+v", c)
	}
	r.Header.Set("Authorization", "bearer hgr_abc")
	if c := CredentialsFromRequest(r); c.Token != "hgr_abc" {
		t.Fatalf("bearer = %+v", c)
	}
}

func TestHTTPStatus(t *testing.T) {
	if s := HTTPStatus(errors.Join(errors.New("x"), ErrWriteDenied)); s != http.StatusConflict {
		t.Fatalf("status = %d", s)
	}
	if s := HTTPStatus(errors.New("boom")); s != http.StatusInternalServerError {
		t.Fatalf("status = %d", s)
	}
}
