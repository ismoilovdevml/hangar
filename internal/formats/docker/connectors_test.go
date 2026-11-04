package docker_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/docker"
)

// addConnectorRepo adds a hosted docker repository with a connector and
// announces it, as the repository service does.
func (tr *testReg) addConnectorRepo(name string, c core.ConnectorConfig) {
	tr.t.Helper()
	tr.env.AddRepo(tr.t, &core.Repository{Name: name, Format: "docker", Type: core.RepoHosted,
		Config: core.RepoConfig{Connector: &c}})
	_ = tr.env.Events.Publish(tr.t.Context(), core.Event{Type: core.EventRepositoryCreated, Repository: name})
}

func host(h string) reqOpt { return func(r *http.Request) { r.Host = h } }

// challenge parses the Bearer challenge of a 401.
func challenge(t *testing.T, resp *http.Response) (realm, service string) {
	t.Helper()
	for _, v := range resp.Header.Values("WWW-Authenticate") {
		if !strings.HasPrefix(v, "Bearer ") {
			continue
		}
		for part := range strings.SplitSeq(strings.TrimPrefix(v, "Bearer "), ",") {
			k, val, _ := strings.Cut(part, "=")
			val = strings.Trim(val, `"`)
			switch k {
			case "realm":
				realm = val
			case "service":
				service = val
			}
		}
		return realm, service
	}
	t.Fatalf("no Bearer challenge in %v", resp.Header)
	return "", ""
}

func TestConnectorHost(t *testing.T) {
	tr := newTestReg(t)
	tr.addConnectorRepo("docker-cr", core.ConnectorConfig{Host: "cr.example.test"})
	cr := host("cr.example.test")

	// Challenges point at the connector's own realm and service.
	resp, body := tr.do(http.MethodGet, "/v2/", nil, cr)
	tr.expect(resp, body, http.StatusUnauthorized)
	realm, service := challenge(t, resp)
	if realm != "http://cr.example.test/v2/token" || service != "cr.example.test" {
		t.Fatalf("connector challenge realm=%q service=%q", realm, service)
	}
	resp, body = tr.do(http.MethodGet, "/v2/team/app/manifests/1", nil, cr)
	tr.expect(resp, body, http.StatusUnauthorized)
	if !strings.Contains(resp.Header.Get("WWW-Authenticate"), `scope="repository:team/app:pull"`) {
		t.Fatalf("scope uses connector naming: %v", resp.Header.Values("WWW-Authenticate"))
	}

	// Token scopes name images without the repository prefix.
	resp, body = tr.do(http.MethodGet, "/v2/token?service=cr.example.test&scope=repository:team/app:pull,push", nil, cr, alice)
	tr.expect(resp, body, http.StatusOK)
	var tok struct{ Token string }
	_ = json.Unmarshal(body, &tok)
	img := newImage("connector")
	tr.pushImage("team/app", "1", img, cr, bearer(tok.Token))

	// Same content on the main endpoint, with the repository prefix.
	resp, body = tr.do(http.MethodGet, "/v2/docker-cr/team/app/manifests/1", nil, alice)
	tr.expect(resp, body, http.StatusOK)
	if string(body) != string(img.manifest) {
		t.Fatal("connector push not visible on the main endpoint")
	}
	// Tokens are bound to their entry point.
	resp, body = tr.do(http.MethodGet, "/v2/docker-cr/team/app/manifests/1", nil, bearer(tok.Token))
	tr.expect(resp, body, http.StatusUnauthorized)
	main := tr.token("repository:docker-cr/team/app:pull", alice)
	resp, body = tr.do(http.MethodGet, "/v2/team/app/manifests/1", nil, cr, bearer(main))
	tr.expect(resp, body, http.StatusUnauthorized)

	// Uploads keep the connector naming in Location.
	resp, body = tr.do(http.MethodPost, "/v2/team/app/blobs/uploads/", nil, cr, alice)
	tr.expect(resp, body, http.StatusAccepted)
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/v2/team/app/blobs/uploads/") {
		t.Fatalf("upload Location %q", loc)
	}
	// The host is case-insensitive and may carry a port.
	resp, body = tr.do(http.MethodGet, "/v2/team/app/tags/list", nil, host("CR.example.test:443"), alice)
	tr.expect(resp, body, http.StatusOK)
	if !strings.Contains(string(body), `"name":"team/app"`) {
		t.Fatalf("tags = %s", body)
	}
	// Catalog lists only the connector's images, without the prefix.
	tr.pushImage("docker-hosted/elsewhere", "1", newImage("elsewhere"), alice)
	resp, body = tr.do(http.MethodGet, "/v2/_catalog", nil, cr, alice)
	tr.expect(resp, body, http.StatusOK)
	if string(body) != `{"repositories":["team/app"]}` {
		t.Fatalf("connector catalog = %s", body)
	}
	// Other hosts keep the shared naming.
	resp, body = tr.do(http.MethodGet, "/v2/team/app/manifests/1", nil, alice)
	tr.expect(resp, body, http.StatusNotFound)

	// Authorization still applies to the repository behind the connector.
	resp, body = tr.do(http.MethodGet, "/v2/team/app/manifests/1", nil, cr, basic("bob", "secret"))
	tr.expect(resp, body, http.StatusForbidden)
}

// A port connector: everything arriving on its listener is that repository.
// Tokens come from the main realm, which recognises the connector by service.
func TestConnectorPort(t *testing.T) {
	tr := newTestReg(t)
	portSrv := httptest.NewServer(tr.srv.Config.Handler)
	t.Cleanup(portSrv.Close)
	_, p, _ := net.SplitHostPort(strings.TrimPrefix(portSrv.URL, "http://"))
	port := 0
	for _, c := range p {
		port = port*10 + int(c-'0')
	}
	tr.addConnectorRepo("docker-port", core.ConnectorConfig{HTTPPort: port})

	resp, body := tr.do(http.MethodGet, portSrv.URL+"/v2/", nil)
	tr.expect(resp, body, http.StatusUnauthorized)
	realm, service := challenge(t, resp)
	if realm != portSrv.URL+"/v2/token" || !strings.HasSuffix(service, ":"+p) {
		t.Fatalf("port challenge realm=%q service=%q", realm, service)
	}
	// Token from the main endpoint, for the connector's service.
	resp, body = tr.do(http.MethodGet, "/v2/token?service="+service+"&scope=repository:app:pull,push", nil, alice)
	tr.expect(resp, body, http.StatusOK)
	var tok struct{ Token string }
	_ = json.Unmarshal(body, &tok)
	img := newImage("port")
	ptr := &testReg{t: t, env: tr.env, srv: portSrv}
	ptr.pushImage("app", "1", img, bearer(tok.Token))
	resp, body = tr.do(http.MethodGet, "/v2/docker-port/app/manifests/1", nil, alice)
	tr.expect(resp, body, http.StatusOK)
}

// A real client (go-containerregistry) pushes and pulls through a connector
// host name, including the token flow against the connector's realm.
func TestConnectorHostGGCR(t *testing.T) {
	tr := newTestReg(t)
	tr.addConnectorRepo("docker-cr", core.ConnectorConfig{Host: "cr.example.test"})
	addr := strings.TrimPrefix(tr.srv.URL, "http://")
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	tp := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, addr) // cr.example.test resolves to the test server
	}}
	opts := []remote.Option{remote.WithTransport(tp), remote.WithAuth(&authn.Basic{Username: "alice", Password: "secret"})}
	ref, err := name.ParseReference("cr.example.test/team/ggcr:v1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	img, err := random.Image(1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img, opts...); err != nil {
		t.Fatalf("push via connector: %v", err)
	}
	got, err := remote.Image(ref, opts...)
	if err != nil {
		t.Fatalf("pull via connector: %v", err)
	}
	gd, _ := got.Digest()
	wd, _ := img.Digest()
	if gd != wd {
		t.Fatalf("digest %s, want %s", gd, wd)
	}
	if _, err := tr.env.Components.GetComponent(t.Context(), mustRepo(t, tr, "docker-cr").ID, "team", "ggcr", "v1"); err != nil {
		t.Fatalf("not stored in the connector repository: %v", err)
	}
}

func TestConnectorValidate(t *testing.T) {
	tr := newTestReg(t)
	f := docker.New()
	if err := f.Validate(&core.Repository{Name: "x", Format: "docker", Type: core.RepoHosted,
		Config: core.RepoConfig{Connector: &core.ConnectorConfig{Host: "not a host"}}}); err == nil {
		t.Fatal("invalid host accepted")
	}
	// Uniqueness and the base_url host are checked once the format is mounted.
	tr.addConnectorRepo("docker-cr", core.ConnectorConfig{Host: "cr.example.test", HTTPPort: 15000})
	repo := func(name string, c core.ConnectorConfig) *core.Repository {
		return &core.Repository{Name: name, Format: "docker", Type: core.RepoHosted, Config: core.RepoConfig{Connector: &c}}
	}
	baseHost, _, _ := net.SplitHostPort(strings.TrimPrefix(tr.srv.URL, "http://"))
	for _, tc := range []struct {
		repo *core.Repository
		ok   bool
	}{
		{repo("other", core.ConnectorConfig{Host: "CR.example.test"}), false},
		{repo("other", core.ConnectorConfig{HTTPPort: 15000}), false},
		{repo("other", core.ConnectorConfig{Host: baseHost}), false},
		{repo("other", core.ConnectorConfig{Host: "cr2.example.test", HTTPPort: 15001}), true},
		{repo("docker-cr", core.ConnectorConfig{Host: "cr.example.test", HTTPPort: 15000}), true}, // update of the owner
	} {
		err := tr.format.Validate(tc.repo)
		if (err == nil) != tc.ok {
			t.Errorf("Validate(%s %+v) = %v, want ok=%v", tc.repo.Name, *tc.repo.Config.Connector, err, tc.ok)
		}
	}
}

// WatchConnectorPorts reports the ports at start and after changes.
func TestWatchConnectorPorts(t *testing.T) {
	tr := newTestReg(t)
	f := tr.format
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var mu sync.Mutex
	var seen []map[int]string
	go f.WatchConnectorPorts(ctx, func(p map[int]string) {
		mu.Lock()
		seen = append(seen, p)
		mu.Unlock()
	})
	waitFor := func(want map[int]string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := len(seen)
			var last map[int]string
			if n > 0 {
				last = seen[n-1]
			}
			mu.Unlock()
			if n > 0 && len(last) == len(want) {
				ok := true
				for k, v := range want {
					ok = ok && last[k] == v
				}
				if ok {
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("ports %v never reported (seen %v)", want, seen)
	}
	waitFor(map[int]string{})
	tr.addConnectorRepo("docker-port", core.ConnectorConfig{HTTPPort: 15001})
	waitFor(map[int]string{15001: "docker-port"})
}
