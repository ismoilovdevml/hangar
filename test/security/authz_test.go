package security

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.yaml.in/yaml/v3"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/builtin"
)

// How a caller without rights to the victim repositories must be treated.
type policy int

const (
	// public: documented for anonymous use (security: [] in the spec, or a
	// protocol endpoint every client needs before it can authenticate).
	public policy = iota
	// self: any signed-in caller, about itself (session, own tokens).
	self
	// filtered: any signed-in caller; the answer may only contain what the
	// caller can read, so the other-repository token must not see a victim.
	filtered
	// repoRead: needs read access to the addressed repository; the reader
	// has it, the other-repository token does not.
	repoRead
	// privileged: administrators only (system privileges or repo admin);
	// both the reader and the other-repository token are refused.
	privileged
	// readerOwned: addresses a token the reader owns: the reader may, the
	// other-repository token (owned by an administrator but scoped to
	// repositories) may not, although its owner could.
	readerOwned
)

// apiPolicy classifies every operation of api/openapi.yaml. A new operation
// fails TestAPIAuthorizationSweep until it is classified here.
var apiPolicy = map[string]policy{
	"GET /auth/providers":                        public,
	"POST /auth/login":                           public,
	"GET /auth/oidc/start":                       public,
	"GET /auth/oidc/callback":                    public,
	"GET /system/status":                         public,
	"GET /system/version":                        public,
	"POST /auth/logout":                          self,
	"GET /auth/session":                          self,
	"GET /me/client-endpoints":                   self,
	"GET /me/sessions":                           self,
	"DELETE /me/sessions":                        self,
	"DELETE /me/sessions/{sid}":                  self,
	"PUT /me/password":                           self,
	"PUT /me/profile":                            self,
	"GET /roles/{name}":                          privileged,
	"POST /roles/{name}/members":                 privileged,
	"DELETE /roles/{name}/members/{id}":          privileged,
	"GET /users/{id}/permissions":                readerOwned,
	"GET /users/{id}/sessions":                   readerOwned,
	"DELETE /users/{id}/sessions":                readerOwned,
	"DELETE /users/{id}/sessions/{sid}":          privileged,
	"POST /users/{id}/disable":                   privileged,
	"POST /users/{id}/enable":                    privileged,
	"POST /users/{id}/unlock":                    privileged,
	"PUT /users/{id}/password":                   privileged,
	"GET /tokens":                                self,
	"POST /tokens":                               self,
	"GET /formats":                               filtered,
	"GET /metrics/overview":                      filtered,
	"GET /upstreams/health":                      filtered,
	"GET /repositories":                          filtered,
	"GET /components":                            filtered,
	"GET /search":                                filtered,
	"GET /findings":                              filtered,
	"GET /findings/trend":                        filtered,
	"GET /scanning/status":                       filtered,
	"GET /events":                                filtered,
	"GET /repositories/{name}":                   repoRead,
	"GET /components/{id}":                       repoRead,
	"GET /components/{id}/assets":                repoRead,
	"GET /components/{id}/versions":              repoRead,
	"GET /components/{id}/findings":              repoRead,
	"GET /findings/{id}":                         repoRead,
	"GET /components/{id}/dependents":            repoRead,
	"GET /components/{id}/sbom":                  repoRead,
	"GET /components/{id}/info":                  repoRead,
	"GET /components/{id}/readme-images/{sig}":   repoRead,
	"GET /repositories/{name}/browse":            repoRead,
	"GET /assets/{id}":                           repoRead,
	"POST /repositories":                         privileged,
	"PUT /repositories/{name}":                   privileged,
	"DELETE /repositories/{name}":                privileged,
	"POST /repositories/{name}/cache:invalidate": privileged,
	"GET /blob-stores":                           privileged,
	"POST /blob-stores":                          privileged,
	"GET /blob-stores/{name}":                    privileged,
	"PUT /blob-stores/{name}":                    privileged,
	"DELETE /blob-stores/{name}":                 privileged,
	"POST /blob-stores/{name}/move-repositories": privileged,
	"DELETE /components/{id}":                    privileged,
	"DELETE /assets/{id}":                        privileged,
	"POST /findings/{id}/waiver":                 privileged,
	"POST /components/{id}:scan":                 privileged,
	"POST /repositories/{name}:scan":             privileged,
	"DELETE /findings/{id}/waiver":               privileged,
	"GET /cleanup-policies":                      privileged,
	"POST /cleanup-policies":                     privileged,
	"POST /cleanup-policies/preview":             privileged,
	"GET /cleanup-policies/{name}":               privileged,
	"PUT /cleanup-policies/{name}":               privileged,
	"DELETE /cleanup-policies/{name}":            privileged,
	"GET /tasks":                                 privileged,
	"POST /tasks":                                privileged,
	"GET /tasks/{id}":                            privileged,
	"PUT /tasks/{id}":                            privileged,
	"DELETE /tasks/{id}":                         privileged,
	"POST /tasks/{id}/run":                       privileged,
	"POST /tasks/{id}/stop":                      privileged,
	"GET /tasks/{id}/runs":                       privileged,
	"GET /tasks/{id}/runs/{run_id}":              privileged,
	"GET /tasks/{id}/runs/{run_id}/log":          privileged,
	"GET /cleanup-policies/{name}/runs":          privileged,
	"POST /cleanup-policies/{name}/run":          privileged,
	"POST /cleanup-policies/{name}/stop":         privileged,
	"GET /users":                                 privileged,
	"POST /users":                                privileged,
	"GET /users/{id}":                            readerOwned,
	"PUT /users/{id}":                            privileged,
	"DELETE /users/{id}":                         privileged,
	"GET /roles":                                 privileged,
	"POST /roles":                                privileged,
	"PUT /roles/{name}":                          privileged,
	"DELETE /roles/{name}":                       privileged,
	"GET /identity-providers":                    privileged,
	"GET /identity-providers/oidc":               privileged,
	"PUT /identity-providers/oidc":               privileged,
	"POST /identity-providers/oidc:test":         privileged,
	"GET /identity-providers/ldap":               privileged,
	"PUT /identity-providers/ldap":               privileged,
	"POST /identity-providers/ldap:test":         privileged,
	"GET /proxies":                               filtered,
	"POST /proxies":                              privileged,
	"GET /proxies/{name}":                        privileged,
	"PUT /proxies/{name}":                        privileged,
	"DELETE /proxies/{name}":                     privileged,
	"POST /proxies/{name}:test":                  privileged,
	"DELETE /tokens/{id}":                        readerOwned,
	"GET /audit":                                 privileged,
	"GET /audit/export":                          privileged,
	"GET /nodes":                                 privileged,
	"GET /system/nodes":                          privileged,
	"GET /system/health":                         privileged,
	"GET /system/logs":                           privileged,
	"GET /replication/targets":                   privileged,
	"POST /replication/targets":                  privileged,
	"DELETE /replication/targets/{name}":         privileged,
}

// denied are the answers that refuse a caller.
func denied(code int) bool {
	return code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusNotFound
}

// refused are answers that do not serve the request although they are not
// an authorization decision: unsupported method, unimplemented endpoint.
func refused(code int) bool {
	return denied(code) || code == http.StatusMethodNotAllowed || code == http.StatusNotImplemented
}

// victimMarkers must not appear in anything served to a caller without
// access to the victim repositories.
var victimMarkers = []string{"-victim", "-vproxy", "-vgroup"}

func leaks(body []byte) string {
	for _, m := range victimMarkers {
		if strings.Contains(string(body), m) {
			return m
		}
	}
	return ""
}

func loadSpecPaths(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	return spec.Paths
}

// apiTargets returns real ids for path parameters, so requests address
// existing resources that belong to the administrator and the victims.
func apiTargets(t *testing.T, h *harness) map[string]string {
	t.Helper()
	var users struct {
		Items []struct{ ID, Username string } `json:"items"`
	}
	_ = json.Unmarshal(h.must(admin, http.MethodGet, "/api/v1/users", nil, http.StatusOK), &users)
	var comps struct {
		Items []struct{ ID string } `json:"items"`
	}
	_ = json.Unmarshal(h.must(admin, http.MethodGet, "/api/v1/components?repository=maven-victim", nil, http.StatusOK), &comps)
	// A deletable blob store and a cleanup policy with its task, so that a
	// missing check would really change something.
	h.must(admin, http.MethodPost, "/api/v1/blob-stores",
		map[string]any{"name": "sweep-empty", "kind": "fs", "fs": map[string]any{"path": "/var/empty/hangar-sweep"}}, http.StatusCreated)
	policy := map[string]any{"name": "nightly", "criteria": map[string]any{"older_than_days": 36500},
		"repositories": []string{"raw-other"}, "schedule": nil}
	var prev struct {
		PreviewToken string `json:"preview_token"`
	}
	_ = json.Unmarshal(h.must(admin, http.MethodPost, "/api/v1/cleanup-policies/preview", policy, http.StatusOK), &prev)
	policy["preview_token"] = prev.PreviewToken
	var pol struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(h.must(admin, http.MethodPost, "/api/v1/cleanup-policies", policy, http.StatusCreated), &pol)
	out := map[string]string{
		"/tasks/{id}":                 pol.TaskID,
		"/repositories/{name}":        "raw-victim",
		"/blob-stores/{name}":         "sweep-empty",
		"/roles/{name}":               "Admin",
		"/cleanup-policies/{name}":    "nightly",
		"/replication/targets/{name}": "edge",
		"{id}":                        "00000000-0000-4000-8000-000000000000",
	}
	for _, u := range users.Items {
		if u.Username == readerUser {
			out["/users/{id}"] = u.ID
		}
	}
	out["/tokens/{id}"] = h.readerSpareID
	if len(comps.Items) == 0 || out["/users/{id}"] == "" || out["/tokens/{id}"] == "" {
		t.Fatalf("fixtures incomplete: users %+v components %+v spare token %q", users, comps, h.readerSpareID)
	}
	out["/components/{id}"] = comps.Items[0].ID
	return out
}

func fillAPIPath(p string, ids map[string]string) string {
	for prefix, v := range ids {
		if strings.HasPrefix(prefix, "/") {
			param := prefix[strings.LastIndex(prefix, "/")+1:]
			if strings.HasPrefix(p, prefix[:strings.LastIndex(prefix, "/")+1]+param) {
				p = strings.Replace(p, param, v, 1)
			}
		}
	}
	for _, param := range []string{"{id}", "{run_id}", "{sid}"} {
		p = strings.ReplaceAll(p, param, ids["{id}"])
	}
	return strings.ReplaceAll(p, "{name}", "x")
}

// TestAPIAuthorizationSweep calls every operation of api/openapi.yaml as an
// anonymous caller, as a reader and with a token scoped to other
// repositories, and fails on any answer that serves a caller who must be
// refused.
func TestAPIAuthorizationSweep(t *testing.T) {
	h := server(t)
	ids := apiTargets(t, h)
	paths := loadSpecPaths(t)
	var ops []string
	for p, item := range paths {
		methods, _ := item.(map[string]any)
		for m := range methods {
			switch m {
			case "get", "post", "put", "delete", "patch":
				ops = append(ops, strings.ToUpper(m)+" "+p)
			}
		}
	}
	sort.Strings(ops)
	for _, op := range ops {
		if _, ok := apiPolicy[op]; !ok {
			t.Errorf("%s is not classified in apiPolicy: decide who may call it", op)
		}
	}
	for op := range apiPolicy {
		if !slices.Contains(ops, op) {
			t.Errorf("apiPolicy lists %s, which api/openapi.yaml does not define", op)
		}
	}

	callers := []cred{anonymous, h.reader(), h.other()}
	checked := 0
	for _, op := range ops {
		pol := apiPolicy[op]
		method, p, _ := strings.Cut(op, " ")
		path := "/api/v1" + fillAPIPath(p, ids)
		var body any
		if method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch {
			body = map[string]any{}
		}
		for _, c := range callers {
			if c.name != anonymous.name && (op == "POST /auth/logout" ||
				(method != http.MethodGet && strings.HasPrefix(p, "/me/"))) {
				continue // would end or change the caller's own sessions and account
			}
			r, err := h.do(c, method, path, body)
			if err != nil {
				t.Errorf("%s as %s: %v", op, c.name, err)
				continue
			}
			checked++
			if msg := judgeAPI(pol, c.name, r); msg != "" {
				t.Errorf("%s (%s) as %s: %s: %d %.200s", op, path, c.name, msg, r.status, r.body)
			}
		}
	}
	t.Logf("checked %d API calls over %d operations", checked, len(ops))
}

func judgeAPI(pol policy, caller string, r response) string {
	ok := r.status >= 200 && r.status < 400
	switch {
	case pol == public:
		return ""
	case caller == anonymous.name:
		if r.status != http.StatusUnauthorized {
			return "anonymous caller not refused with 401"
		}
	case pol == self:
	case pol == filtered:
		if caller == "other-repo token" && ok {
			if m := leaks(r.body); m != "" {
				return "listing leaks " + m
			}
		}
		if r.status >= 500 && r.status != http.StatusNotImplemented {
			return "server error"
		}
	case (pol == repoRead || pol == readerOwned) && caller == "reader":
		if r.status >= 500 && r.status != http.StatusNotImplemented {
			return "server error"
		}
	default: // privileged, or repoRead/readerOwned for the other-repository token
		if ok {
			return "unexpected success"
		}
		// 400: the (empty) body was rejected and nothing was done.
		if !refused(r.status) && r.status != http.StatusBadRequest {
			return "unexpected answer"
		}
	}
	return ""
}

// formatRequest is one package-protocol request; R in Path is replaced by
// the target repository name.
type formatRequest struct {
	Method, Path string
	Body         string
	// Write requests change content: the reader must be refused too.
	Write bool
	// Public requests are protocol endpoints every client may call
	// anonymously (registry token service, npm ping/login); they never
	// return repository content.
	Public bool
	// Self requests are about the caller (npm whoami, registry ping).
	Self bool
	// Filtered listings may answer, without the victims.
	Filtered bool
}

const zeroDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// formatRequests lists the endpoints of every format. TestFormatEndpoints-
// Covered fails when a format registers a route none of them reaches.
var formatRequests = map[string][]formatRequest{
	"docker": {
		{Method: "GET", Path: "/v2/", Self: true},
		{Method: "GET", Path: "/v2", Self: true},
		{Method: "HEAD", Path: "/v2", Self: true},
		{Method: "PUT", Path: "/v2", Write: true},
		{Method: "POST", Path: "/v2", Write: true},
		{Method: "PATCH", Path: "/v2", Write: true},
		{Method: "DELETE", Path: "/v2", Write: true},
		{Method: "GET", Path: "/v2/_catalog", Filtered: true},
		{Method: "GET", Path: "/v2/token?service=hangar&scope=repository:R/app:pull,push", Public: true},
		{Method: "POST", Path: "/v2/token", Body: "grant_type=password&service=hangar&scope=repository:R/app:pull", Public: true},
		{Method: "GET", Path: "/v2/R/app/manifests/latest"},
		{Method: "HEAD", Path: "/v2/R/app/manifests/latest"},
		{Method: "PUT", Path: "/v2/R/app/manifests/latest", Body: "{}", Write: true},
		{Method: "DELETE", Path: "/v2/R/app/manifests/" + zeroDigest, Write: true},
		{Method: "GET", Path: "/v2/R/app/tags/list"},
		{Method: "GET", Path: "/v2/R/app/referrers/" + zeroDigest},
		{Method: "GET", Path: "/v2/R/app/blobs/" + zeroDigest},
		{Method: "HEAD", Path: "/v2/R/app/blobs/" + zeroDigest},
		{Method: "DELETE", Path: "/v2/R/app/blobs/" + zeroDigest, Write: true},
		{Method: "POST", Path: "/v2/R/app/blobs/uploads/", Write: true},
		{Method: "POST", Path: "/v2/R/app/blobs/uploads/?mount=" + zeroDigest + "&from=docker-other/app", Write: true},
		{Method: "PATCH", Path: "/v2/R/app/blobs/uploads/00000000-0000-4000-8000-000000000000", Body: "x", Write: true},
		{Method: "PUT", Path: "/v2/R/app/blobs/uploads/00000000-0000-4000-8000-000000000000?digest=" + zeroDigest, Write: true},
		{Method: "GET", Path: "/v2/R/app/blobs/uploads/00000000-0000-4000-8000-000000000000", Write: true},
		{Method: "DELETE", Path: "/v2/R/app/blobs/uploads/00000000-0000-4000-8000-000000000000", Write: true},
		{Method: "GET", Path: "/repository/R/v2/"},
		{Method: "PUT", Path: "/repository/R/anything", Write: true},
		{Method: "POST", Path: "/repository/R/anything", Write: true},
		{Method: "DELETE", Path: "/repository/R/anything", Write: true},
		{Method: "PATCH", Path: "/repository/R/anything", Write: true},
		{Method: "HEAD", Path: "/repository/R/anything"},
	},
	"npm": {
		{Method: "GET", Path: "/repository/R/", Public: true}, // registry root: {"db_name": <repository>}
		{Method: "HEAD", Path: "/repository/R/", Public: true},
		{Method: "PUT", Path: "/repository/R/", Write: true},
		{Method: "POST", Path: "/repository/R/", Write: true},
		{Method: "PATCH", Path: "/repository/R/", Write: true},
		{Method: "DELETE", Path: "/repository/R/", Write: true},
		{Method: "GET", Path: "/repository/R/-/ping", Public: true},
		{Method: "PUT", Path: "/repository/R/-/user/org.couchdb.user:mallory", Body: `{"name":"mallory","password":"guess"}`, Public: true},
		{Method: "DELETE", Path: "/repository/R/-/user/token/abc", Public: true}, // npm logout: no-op
		{Method: "GET", Path: "/repository/R/-/whoami", Self: true},
		{Method: "GET", Path: "/repository/R/pkg"},
		{Method: "HEAD", Path: "/repository/R/pkg"},
		{Method: "GET", Path: "/repository/R/@acme%2fpkg"},
		{Method: "GET", Path: "/repository/R/pkg/1.0.0"},
		{Method: "GET", Path: "/repository/R/pkg/-/pkg-1.0.0.tgz"},
		{Method: "GET", Path: "/repository/R/-/package/pkg/dist-tags"},
		{Method: "POST", Path: "/repository/R/-/npm/v1/security/advisories/bulk", Body: `{"pkg":["1.0.0"]}`},
		{Method: "PUT", Path: "/repository/R/pkg", Body: "{}", Write: true},
		{Method: "PUT", Path: "/repository/R/pkg/-rev/1", Body: "{}", Write: true},
		{Method: "DELETE", Path: "/repository/R/pkg/-rev/1", Write: true},
		{Method: "DELETE", Path: "/repository/R/pkg/-/pkg-1.0.0.tgz/-rev/1", Write: true},
		{Method: "PUT", Path: "/repository/R/-/package/pkg/dist-tags/beta", Body: `"1.0.0"`, Write: true},
		{Method: "DELETE", Path: "/repository/R/-/package/pkg/dist-tags/beta", Write: true},
		{Method: "PATCH", Path: "/repository/R/pkg", Body: "{}", Write: true},
	},
	"maven": {
		{Method: "GET", Path: "/repository/R/com/acme/lib/1.0/lib-1.0.jar"},
		{Method: "HEAD", Path: "/repository/R/com/acme/lib/1.0/lib-1.0.jar"},
		{Method: "GET", Path: "/repository/R/com/acme/lib/1.0/lib-1.0.pom.sha1"},
		{Method: "GET", Path: "/repository/R/com/acme/lib/maven-metadata.xml"},
		{Method: "GET", Path: "/repository/R/com/acme/"},
		{Method: "PUT", Path: "/repository/R/com/acme/lib/2.0/lib-2.0.jar", Body: "x", Write: true},
		{Method: "DELETE", Path: "/repository/R/com/acme/lib/1.0/lib-1.0.jar", Write: true},
	},
	"raw": {
		{Method: "GET", Path: "/repository/R/dir/file.txt"},
		{Method: "HEAD", Path: "/repository/R/dir/file.txt"},
		{Method: "GET", Path: "/repository/R/dir/"},
		{Method: "GET", Path: "/repository/R/"},
		{Method: "GET", Path: "/repository/R/?format=json"},
		{Method: "PUT", Path: "/repository/R/dir/new.txt", Body: "x", Write: true},
		{Method: "POST", Path: "/repository/R/dir/", Body: "x", Write: true},
		{Method: "PATCH", Path: "/repository/R/dir/file.txt", Body: "x", Write: true},
		{Method: "DELETE", Path: "/repository/R/dir/file.txt", Write: true},
	},
	"pypi": {
		{Method: "GET", Path: "/repository/R/simple"},
		{Method: "GET", Path: "/repository/R/simple/"},
		{Method: "GET", Path: "/repository/R/simple/pkg"},
		{Method: "GET", Path: "/repository/R/simple/pkg/"},
		{Method: "GET", Path: "/repository/R/packages/pkg/pkg-1.0.tar.gz"},
		{Method: "HEAD", Path: "/repository/R/packages/pkg/pkg-1.0.tar.gz"},
		{Method: "POST", Path: "/repository/R/", Body: "x", Write: true},
		{Method: "POST", Path: "/repository/R/legacy/", Body: "x", Write: true},
		{Method: "POST", Path: "/repository/R/yank/pkg/1.0", Write: true},
		{Method: "DELETE", Path: "/repository/R/yank/pkg/1.0", Write: true},
	},
	"nuget": {
		// The service index only lists this repository's endpoint URLs;
		// clients read it before they are challenged for credentials.
		{Method: "GET", Path: "/repository/R/index.json", Public: true},
		{Method: "HEAD", Path: "/repository/R/index.json", Public: true},
		{Method: "GET", Path: "/repository/R/query?q=pkg"},
		{Method: "HEAD", Path: "/repository/R/query?q=pkg"},
		{Method: "GET", Path: "/repository/R/v3-flatcontainer/pkg/index.json"},
		{Method: "HEAD", Path: "/repository/R/v3-flatcontainer/pkg/index.json"},
		{Method: "GET", Path: "/repository/R/v3-flatcontainer/pkg/1.0.0/pkg.1.0.0.nupkg"},
		{Method: "HEAD", Path: "/repository/R/v3-flatcontainer/pkg/1.0.0/pkg.1.0.0.nupkg"},
		{Method: "GET", Path: "/repository/R/v3/registration/pkg/index.json"},
		{Method: "HEAD", Path: "/repository/R/v3/registration/pkg/index.json"},
		{Method: "GET", Path: "/repository/R/v3/registration/pkg/page/1.0.0/2.0.0.json"},
		{Method: "HEAD", Path: "/repository/R/v3/registration/pkg/page/1.0.0/2.0.0.json"},
		{Method: "GET", Path: "/repository/R/v3/registration/pkg/1.0.0.json"},
		{Method: "HEAD", Path: "/repository/R/v3/registration/pkg/1.0.0.json"},
		{Method: "PUT", Path: "/repository/R/api/v2/package", Body: "x", Write: true},
		{Method: "PUT", Path: "/repository/R/api/v2/package/", Body: "x", Write: true},
		{Method: "DELETE", Path: "/repository/R/api/v2/package/pkg/1.0.0", Write: true},
		{Method: "POST", Path: "/repository/R/api/v2/package/pkg/1.0.0", Write: true},
		{Method: "PUT", Path: "/repository/R/api/v2/symbolpackage", Body: "x", Write: true},
		{Method: "PUT", Path: "/repository/R/api/v2/symbolpackage/", Body: "x", Write: true},
	},
}

// sweptMethods are the methods package clients use; chi's catch-all
// routes also list CONNECT, TRACE, QUERY and OPTIONS.
var sweptMethods = []string{"GET", "HEAD", "PUT", "POST", "PATCH", "DELETE"}

// TestFormatEndpointsCovered fails when a format registers a route that no
// entry of formatRequests reaches, so new endpoints join the sweep.
func TestFormatEndpointsCovered(t *testing.T) {
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range reg.All() {
		reqs, ok := formatRequests[f.Name()]
		if !ok {
			t.Errorf("format %s has no entry in formatRequests", f.Name())
			continue
		}
		sub := chi.NewRouter()
		f.Mount(sub, core.Deps{})
		root := chi.NewRouter()
		if rm, ok := f.(core.RootMounter); ok {
			rm.MountRoot(root, core.Deps{})
		}
		covered := map[string]bool{}
		for _, rq := range reqs {
			path := strings.ReplaceAll(rq.Path, "R", f.Name()+"-victim")
			path, _, _ = strings.Cut(path, "?")
			var router *chi.Mux
			if rest, ok := strings.CutPrefix(path, "/repository/"+f.Name()+"-victim"); ok {
				router, path = sub, rest
				if path == "" {
					path = "/"
				}
			} else {
				router = root
			}
			rctx := chi.NewRouteContext()
			if router.Match(rctx, rq.Method, path) {
				covered[rq.Method+" "+strings.TrimSuffix(rctx.RoutePattern(), "/")] = true
			}
		}
		for _, r := range []*chi.Mux{sub, root} {
			_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
				if slices.Contains(sweptMethods, method) && !covered[method+" "+strings.TrimSuffix(route, "/")] {
					t.Errorf("%s: %s %s is not reached by any formatRequests entry", f.Name(), method, route)
				}
				return nil
			})
		}
	}
}

// TestFormatAuthorizationSweep sends every format request to the private
// victim repositories (hosted, and reads also through a proxy and a group
// over them) as an anonymous caller, as a reader and with a token scoped to
// other repositories.
func TestFormatAuthorizationSweep(t *testing.T) {
	h := server(t)
	checked := 0
	for _, f := range formatsUnderTest {
		for _, rq := range formatRequests[f] {
			targets := []string{f + "-victim"}
			if !rq.Write && !rq.Public && !rq.Self {
				targets = append(targets, f+"-vgroup", f+"-vproxy")
			}
			for _, repo := range targets {
				path := strings.ReplaceAll(rq.Path, "R", repo)
				var body any
				if rq.Body != "" {
					body = rq.Body
				}
				for _, c := range []cred{anonymous, h.reader(), h.other()} {
					hdr := []string{}
					if rq.Method == http.MethodPost && strings.HasSuffix(path, "/v2/token") {
						hdr = []string{"Content-Type", "application/x-www-form-urlencoded"}
					}
					r, err := h.do(c, rq.Method, path, body, hdr...)
					if err != nil {
						t.Errorf("%s %s as %s: %v", rq.Method, path, c.name, err)
						continue
					}
					checked++
					if msg := judgeFormat(rq, path, c.name, r); msg != "" {
						t.Errorf("%s %s as %s: %s: %d %.200s", rq.Method, path, c.name, msg, r.status, r.body)
					}
				}
			}
		}
	}
	t.Logf("checked %d package requests", checked)
}

// canonicalRedirect reports a redirect to the same path with a trailing
// slash: it serves nothing, and the target is checked like any request.
func canonicalRedirect(path string, r response) bool {
	if r.status != http.StatusMovedPermanently && r.status != http.StatusPermanentRedirect {
		return false
	}
	path, _, _ = strings.Cut(path, "?")
	return r.header.Get("Location") == path+"/"
}

func judgeFormat(rq formatRequest, path, caller string, r response) string {
	ok := r.status >= 200 && r.status < 400
	switch {
	case rq.Public:
		return ""
	case r.status == http.StatusInternalServerError:
		return "server error"
	case canonicalRedirect(path, r):
		return ""
	case caller == anonymous.name:
		if rq.Filtered && ok {
			if m := leaks(r.body); m != "" {
				return "anonymous listing leaks " + m
			}
			return ""
		}
		if !refused(r.status) {
			return "anonymous caller not refused"
		}
	case rq.Self:
	case rq.Filtered:
		if ok && caller == "other-repo token" {
			if m := leaks(r.body); m != "" {
				return "listing leaks " + m
			}
		}
	case caller == "reader" && !rq.Write:
		// The reader may read every repository (a proxy over an unreachable
		// remote answers 502).
	default:
		if !refused(r.status) {
			return "caller not refused"
		}
	}
	return ""
}
