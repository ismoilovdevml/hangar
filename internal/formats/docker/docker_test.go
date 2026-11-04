package docker_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/docker"
)

func TestPingChallenge(t *testing.T) {
	tr := newTestReg(t)
	resp, body := tr.do(http.MethodGet, "/v2/", nil)
	tr.expect(resp, body, http.StatusUnauthorized)
	ch := resp.Header.Values("WWW-Authenticate")
	if len(ch) != 2 || !strings.HasPrefix(ch[0], `Bearer realm="`+tr.srv.URL+`/v2/token"`) || !strings.HasPrefix(ch[1], "Basic") {
		t.Fatalf("challenges = %q", ch)
	}
	if v := resp.Header.Get("Docker-Distribution-API-Version"); v != "registry/2.0" {
		t.Fatalf("api version header = %q", v)
	}
	resp, body = tr.do(http.MethodGet, "/v2/", nil, alice)
	tr.expect(resp, body, http.StatusOK)
	resp, body = tr.do(http.MethodGet, "/v2/", nil, basic("alice", "wrong"))
	tr.expect(resp, body, http.StatusUnauthorized)
}

func TestNameRouting(t *testing.T) {
	tr := newTestReg(t)
	tests := []struct {
		path string
		code int
		err  string
	}{
		{"/v2/docker-hosted/manifests/latest", http.StatusBadRequest, "NAME_INVALID"}, // no image path
		{"/v2/Docker-hosted/app/manifests/latest", http.StatusBadRequest, "NAME_INVALID"},
		{"/v2/nope/app/manifests/latest", http.StatusNotFound, "NAME_UNKNOWN"},
		{"/v2/docker-hosted/app/manifests/latest", http.StatusNotFound, "MANIFEST_UNKNOWN"},
		{"/v2/docker-hosted/team/app/blobs/sha256:" + strings.Repeat("a", 64), http.StatusNotFound, "BLOB_UNKNOWN"},
		{"/v2/docker-hosted/app/blobs/sha256:xyz", http.StatusBadRequest, "DIGEST_INVALID"},
		{"/v2/docker-hosted/app/tags/list", http.StatusNotFound, "NAME_UNKNOWN"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			resp, body := tr.do(http.MethodGet, tc.path, nil, alice)
			if resp.StatusCode != tc.code || errCode(body) != tc.err {
				t.Fatalf("= %d %s, want %d %s", resp.StatusCode, body, tc.code, tc.err)
			}
		})
	}
}

func TestTokenFlow(t *testing.T) {
	tr := newTestReg(t)
	tok := tr.token("repository:docker-hosted/app:pull,push", alice)

	// The token works for push, without other credentials.
	img := newImage("tok")
	tr.pushImage("docker-hosted/app", "v1", img, bearer(tok))
	resp, body := tr.do(http.MethodGet, "/v2/docker-hosted/app/manifests/v1", nil, bearer(tok))
	tr.expect(resp, body, http.StatusOK)

	// Out of scope: another repository -> insufficient_scope challenge.
	resp, body = tr.do(http.MethodGet, "/v2/docker-public/app/tags/list", nil, bearer(tok))
	tr.expect(resp, body, http.StatusUnauthorized)
	if ch := resp.Header.Get("WWW-Authenticate"); !strings.Contains(ch, `error="insufficient_scope"`) || !strings.Contains(ch, `scope="repository:docker-public/app:pull"`) {
		t.Fatalf("challenge = %q", ch)
	}

	// bob may only pull: push is dropped from the token and the push is denied.
	btok := tr.token("repository:docker-hosted/app:pull,push", basic("bob", "secret"))
	resp, body = tr.do(http.MethodGet, "/v2/docker-hosted/app/manifests/v1", nil, bearer(btok))
	tr.expect(resp, body, http.StatusOK)
	resp, body = tr.do(http.MethodPost, "/v2/docker-hosted/app/blobs/uploads/", nil, bearer(btok))
	tr.expect(resp, body, http.StatusForbidden)

	// Anonymous tokens grant pull on anonymous-read repositories only.
	tr.pushImage("docker-public/app", "v1", img, alice)
	atok := tr.token("repository:docker-public/app:pull+repository:docker-hosted/app:pull")
	resp, body = tr.do(http.MethodGet, "/v2/docker-public/app/manifests/v1", nil, bearer(atok))
	tr.expect(resp, body, http.StatusOK)
	resp, body = tr.do(http.MethodGet, "/v2/docker-hosted/app/manifests/v1", nil, bearer(atok))
	tr.expect(resp, body, http.StatusUnauthorized)

	// Wrong credentials at the token endpoint.
	resp, body = tr.do(http.MethodGet, "/v2/token?scope=repository:docker-hosted/app:pull", nil, basic("alice", "nope"))
	tr.expect(resp, body, http.StatusUnauthorized)

	// Tampered and expired tokens are rejected.
	resp, body = tr.do(http.MethodGet, "/v2/docker-hosted/app/manifests/v1", nil, bearer(tok[:len(tok)-2]+"xx"))
	tr.expect(resp, body, http.StatusUnauthorized)
	tr.now = tr.now.Add(docker.DefaultTokenTTL + time.Minute)
	resp, body = tr.do(http.MethodGet, "/v2/docker-hosted/app/manifests/v1", nil, bearer(tok))
	tr.expect(resp, body, http.StatusUnauthorized)
}

func TestTokenPasswordGrant(t *testing.T) {
	tr := newTestReg(t)
	form := "grant_type=password&username=alice&password=secret&service=x&client_id=test&scope=repository:docker-hosted/app:pull"
	resp, body := tr.do(http.MethodPost, "/v2/token", []byte(form), header("Content-Type", "application/x-www-form-urlencoded"))
	tr.expect(resp, body, http.StatusOK)
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" || tok.ExpiresIn <= 0 {
		t.Fatalf("token = %s", body)
	}
	resp, body = tr.do(http.MethodPost, "/v2/token", []byte("grant_type=refresh_token&refresh_token=x"), header("Content-Type", "application/x-www-form-urlencoded"))
	tr.expect(resp, body, http.StatusBadRequest)
}

func TestChunkedUpload(t *testing.T) {
	tr := newTestReg(t)
	data := []byte("hello chunked world, this is a blob")
	d := core.SHA256Of(data)
	resp, body := tr.do(http.MethodPost, "/v2/docker-hosted/app/blobs/uploads/", nil, alice)
	tr.expect(resp, body, http.StatusAccepted)
	loc, uuid := resp.Header.Get("Location"), resp.Header.Get("Docker-Upload-UUID")
	if loc == "" || uuid == "" || !strings.Contains(loc, uuid) {
		t.Fatalf("Location %q UUID %q", loc, uuid)
	}

	resp, body = tr.do(http.MethodPatch, loc, data[:10], alice, header("Content-Range", "0-9"))
	tr.expect(resp, body, http.StatusAccepted)
	if r := resp.Header.Get("Range"); r != "0-9" {
		t.Fatalf("Range = %q", r)
	}
	loc2 := resp.Header.Get("Location")

	// Out of order -> 416 with the current offset.
	resp, body = tr.do(http.MethodPatch, loc2, data[20:], alice, header("Content-Range", fmt.Sprintf("20-%d", len(data)-1)))
	tr.expect(resp, body, http.StatusRequestedRangeNotSatisfiable)
	if r := resp.Header.Get("Range"); r != "0-9" {
		t.Fatalf("Range after 416 = %q", r)
	}
	// Status.
	resp, body = tr.do(http.MethodGet, loc2, nil, alice)
	tr.expect(resp, body, http.StatusNoContent)
	if r := resp.Header.Get("Range"); r != "0-9" {
		t.Fatalf("status Range = %q", r)
	}
	// Second chunk without Content-Range (streamed), final chunk in the PUT.
	resp, body = tr.do(http.MethodPatch, loc2, data[10:20], alice)
	tr.expect(resp, body, http.StatusAccepted)
	loc3 := resp.Header.Get("Location")
	// Forged state is unknown.
	resp, body = tr.do(http.MethodPut, strings.Replace(loc3, "_state=", "_state=00", 1)+"&digest="+string(d), data[20:], alice)
	tr.expect(resp, body, http.StatusNotFound)
	if errCode(body) != "BLOB_UPLOAD_UNKNOWN" {
		t.Fatalf("code = %s", body)
	}
	// Wrong digest.
	resp, body = tr.do(http.MethodPut, loc3+"&digest="+string(core.SHA256Of([]byte("x"))), data[20:], alice)
	tr.expect(resp, body, http.StatusBadRequest)
	if errCode(body) != "DIGEST_INVALID" {
		t.Fatalf("code = %s", body)
	}
	resp, body = tr.do(http.MethodPut, loc3+"&digest="+string(d), data[20:], alice)
	tr.expect(resp, body, http.StatusCreated)
	if resp.Header.Get("Location") != "/v2/docker-hosted/app/blobs/"+string(d) || resp.Header.Get("Docker-Content-Digest") != string(d) {
		t.Fatalf("headers %v", resp.Header)
	}

	// HEAD, GET and Range GET.
	resp, body = tr.do(http.MethodHead, "/v2/docker-hosted/app/blobs/"+string(d), nil, alice)
	tr.expect(resp, body, http.StatusOK)
	if resp.ContentLength != int64(len(data)) {
		t.Fatalf("HEAD length = %d", resp.ContentLength)
	}
	resp, body = tr.do(http.MethodGet, "/v2/docker-hosted/app/blobs/"+string(d), nil, alice)
	tr.expect(resp, body, http.StatusOK)
	if string(body) != string(data) {
		t.Fatalf("blob = %q", body)
	}
	resp, body = tr.do(http.MethodGet, "/v2/docker-hosted/app/blobs/"+string(d), nil, alice, header("Range", "bytes=6-12"))
	tr.expect(resp, body, http.StatusPartialContent)
	if string(body) != string(data[6:13]) {
		t.Fatalf("range = %q", body)
	}
	// Session upload by another image path is unknown.
	resp, body = tr.do(http.MethodPut, strings.Replace(loc3, "/app/", "/other/", 1)+"&digest="+string(d), nil, alice)
	tr.expect(resp, body, http.StatusNotFound)
	// Cancel.
	resp, body = tr.do(http.MethodDelete, loc3, nil, alice)
	tr.expect(resp, body, http.StatusNoContent)
}

func TestMonolithicUploadDigestMismatch(t *testing.T) {
	tr := newTestReg(t)
	resp, body := tr.do(http.MethodPost, "/v2/docker-hosted/app/blobs/uploads/?digest="+string(core.SHA256Of([]byte("a"))), []byte("b"), alice)
	tr.expect(resp, body, http.StatusBadRequest)
	if errCode(body) != "DIGEST_INVALID" {
		t.Fatalf("code = %s", body)
	}
}

func TestCrossRepoMount(t *testing.T) {
	tr := newTestReg(t)
	d := tr.pushBlob("docker-hosted/base", []byte("shared layer"), alice)
	blobs := tr.env.Blob.Len()

	resp, body := tr.do(http.MethodPost, "/v2/docker-public/app/blobs/uploads/?mount="+string(d)+"&from=docker-hosted/base", nil, alice)
	tr.expect(resp, body, http.StatusCreated)
	if resp.Header.Get("Docker-Content-Digest") != string(d) {
		t.Fatalf("headers %v", resp.Header)
	}
	if tr.env.Blob.Len() != blobs {
		t.Fatal("mount copied bytes within one blob store")
	}
	resp, body = tr.do(http.MethodGet, "/v2/docker-public/app/blobs/"+string(d), nil)
	tr.expect(resp, body, http.StatusOK)

	// bob cannot write docker-public; an unknown source falls back to an upload session.
	resp, body = tr.do(http.MethodPost, "/v2/docker-hosted/app/blobs/uploads/?mount="+string(core.SHA256Of([]byte("zz")))+"&from=docker-hosted/base", nil, alice)
	tr.expect(resp, body, http.StatusAccepted)
}

func TestManifestLifecycle(t *testing.T) {
	tr := newTestReg(t)
	img := newImage("one")
	name := "docker-hosted/team/app"

	// Referenced blobs must exist.
	resp, body := tr.do(http.MethodPut, "/v2/"+name+"/manifests/v1", img.manifest, alice, header("Content-Type", "application/vnd.oci.image.manifest.v1+json"))
	tr.expect(resp, body, http.StatusBadRequest)
	if errCode(body) != "MANIFEST_BLOB_UNKNOWN" {
		t.Fatalf("code = %s", body)
	}
	tr.pushImage(name, "v1", img, alice)
	tr.pushImage(name, "v2", img, alice)
	img3 := newImage("three")
	tr.pushImage(name, "a-first", img3, alice)

	for _, ref := range []string{"v1", string(img.digest)} {
		resp, body = tr.do(http.MethodGet, "/v2/"+name+"/manifests/"+ref, nil, alice)
		tr.expect(resp, body, http.StatusOK)
		if string(body) != string(img.manifest) || resp.Header.Get("Content-Type") != "application/vnd.oci.image.manifest.v1+json" ||
			resp.Header.Get("Docker-Content-Digest") != string(img.digest) {
			t.Fatalf("GET %s: %v %s", ref, resp.Header, body)
		}
		resp, body = tr.do(http.MethodHead, "/v2/"+name+"/manifests/"+ref, nil, alice)
		tr.expect(resp, body, http.StatusOK)
		if resp.ContentLength != int64(len(img.manifest)) {
			t.Fatalf("HEAD length %d", resp.ContentLength)
		}
	}

	// Component per tag.
	if _, err := tr.env.Components.GetComponent(t.Context(), mustRepo(t, tr, "docker-hosted").ID, "team", "app", "v1"); err != nil {
		t.Fatalf("component: %v", err)
	}

	// Tags pagination.
	resp, body = tr.do(http.MethodGet, "/v2/"+name+"/tags/list?n=2", nil, alice)
	tr.expect(resp, body, http.StatusOK)
	var tl struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	_ = json.Unmarshal(body, &tl)
	if tl.Name != name || !slices.Equal(tl.Tags, []string{"a-first", "v1"}) {
		t.Fatalf("tags = %s", body)
	}
	if link := resp.Header.Get("Link"); link != `</v2/`+name+`/tags/list?last=v1&n=2>; rel="next"` {
		t.Fatalf("Link = %q", link)
	}
	resp, body = tr.do(http.MethodGet, "/v2/"+name+"/tags/list?n=2&last=v1", nil, alice)
	tr.expect(resp, body, http.StatusOK)
	_ = json.Unmarshal(body, &tl)
	if !slices.Equal(tl.Tags, []string{"v2"}) || resp.Header.Get("Link") != "" {
		t.Fatalf("page 2 = %s link %q", body, resp.Header.Get("Link"))
	}
	resp, body = tr.do(http.MethodGet, "/v2/"+name+"/tags/list?n=0", nil, alice)
	tr.expect(resp, body, http.StatusOK)
	_ = json.Unmarshal(body, &tl)
	if len(tl.Tags) != 0 || resp.Header.Get("Link") != "" {
		t.Fatalf("n=0: %s", body)
	}

	// Index referencing a missing child is rejected; with children present it is accepted.
	idx := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[`+
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":%d,"platform":{"os":"linux","architecture":"amd64"}},`+
		`{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":1,"platform":{"os":"linux","architecture":"arm64"}}]}`,
		img.digest, len(img.manifest), core.SHA256Of([]byte("missing"))))
	resp, body = tr.do(http.MethodPut, "/v2/"+name+"/manifests/multi", idx, alice, header("Content-Type", "application/vnd.oci.image.index.v1+json"))
	tr.expect(resp, body, http.StatusBadRequest)

	// Bad manifests.
	for _, bad := range []struct{ ct, body string }{
		{"application/vnd.oci.image.manifest.v1+json", `not json`},
		{"application/vnd.oci.image.manifest.v1+json", `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`},
		{"application/vnd.oci.image.index.v1+json", string(img.manifest)},
		{"application/vnd.docker.distribution.manifest.v1+prettyjws", `{"schemaVersion":1}`},
	} {
		resp, body = tr.do(http.MethodPut, "/v2/"+name+"/manifests/bad", []byte(bad.body), alice, header("Content-Type", bad.ct))
		if resp.StatusCode != http.StatusBadRequest || errCode(body) != "MANIFEST_INVALID" {
			t.Fatalf("bad manifest %q = %d %s", bad.body, resp.StatusCode, body)
		}
	}
	// PUT by digest must match the content.
	resp, body = tr.do(http.MethodPut, "/v2/"+name+"/manifests/"+string(img3.digest), img.manifest, alice, header("Content-Type", "application/vnd.oci.image.manifest.v1+json"))
	tr.expect(resp, body, http.StatusBadRequest)

	// Delete a tag, then the manifest by digest (removes remaining tags).
	resp, body = tr.do(http.MethodDelete, "/v2/"+name+"/manifests/v2", nil, alice)
	tr.expect(resp, body, http.StatusAccepted)
	resp, body = tr.do(http.MethodGet, "/v2/"+name+"/manifests/v2", nil, alice)
	tr.expect(resp, body, http.StatusNotFound)
	resp, body = tr.do(http.MethodDelete, "/v2/"+name+"/manifests/"+string(img.digest), nil, basic("bob", "secret"))
	tr.expect(resp, body, http.StatusForbidden)
	resp, body = tr.do(http.MethodDelete, "/v2/"+name+"/manifests/"+string(img.digest), nil, alice)
	tr.expect(resp, body, http.StatusAccepted)
	for _, ref := range []string{"v1", string(img.digest)} {
		resp, body = tr.do(http.MethodGet, "/v2/"+name+"/manifests/"+ref, nil, alice)
		tr.expect(resp, body, http.StatusNotFound)
	}
	resp, body = tr.do(http.MethodGet, "/v2/"+name+"/manifests/a-first", nil, alice)
	tr.expect(resp, body, http.StatusOK)

	// Blob delete.
	resp, body = tr.do(http.MethodDelete, "/v2/"+name+"/blobs/"+string(core.SHA256Of(img.layer)), nil, alice)
	tr.expect(resp, body, http.StatusAccepted)
	resp, body = tr.do(http.MethodHead, "/v2/"+name+"/blobs/"+string(core.SHA256Of(img.layer)), nil, alice)
	tr.expect(resp, body, http.StatusNotFound)

	// Audit and events from the service layer.
	if len(tr.env.Audit.Events()) == 0 {
		t.Fatal("no audit events")
	}
}

func TestAllowOnceTags(t *testing.T) {
	tr := newTestReg(t)
	tr.env.AddRepo(t, &core.Repository{Name: "docker-release", Format: "docker", Type: core.RepoHosted,
		Config: core.RepoConfig{Hosted: &core.HostedConfig{WritePolicy: core.WriteAllowOnce}}})
	tr.pushImage("docker-release/app", "1.0", newImage("a"), alice)
	tr.pushImage("docker-release/app", "1.0", newImage("a"), alice) // identical: idempotent
	img := newImage("b")
	tr.pushBlob("docker-release/app", img.config, alice)
	tr.pushBlob("docker-release/app", img.layer, alice)
	resp, body := tr.do(http.MethodPut, "/v2/docker-release/app/manifests/1.0", img.manifest, alice, header("Content-Type", "application/vnd.oci.image.manifest.v1+json"))
	tr.expect(resp, body, http.StatusConflict)
	if errCode(body) != "DENIED" {
		t.Fatalf("code = %s", body)
	}
}

func TestReferrers(t *testing.T) {
	tr := newTestReg(t)
	name := "docker-hosted/app"
	img := newImage("subject")
	tr.pushImage(name, "v1", img, alice)

	empty := []byte("{}")
	tr.pushBlob(name, empty, alice)
	sig := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",`+
		`"artifactType":"application/vnd.dev.cosign.artifact.sig.v1+json",`+
		`"config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":%q,"size":2},"layers":[],`+
		`"subject":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":%d},`+
		`"annotations":{"org.example":"sig"}}`, core.SHA256Of(empty), img.digest, len(img.manifest)))
	sbom := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",`+
		`"config":{"mediaType":"application/spdx+json","digest":%q,"size":2},"layers":[],`+
		`"subject":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":%d}}`, core.SHA256Of(empty), img.digest, len(img.manifest)))
	for _, m := range [][]byte{sig, sbom} {
		d := core.SHA256Of(m)
		resp, body := tr.do(http.MethodPut, "/v2/"+name+"/manifests/"+string(d), m, alice, header("Content-Type", "application/vnd.oci.image.manifest.v1+json"))
		tr.expect(resp, body, http.StatusCreated)
		if resp.Header.Get("OCI-Subject") != string(img.digest) {
			t.Fatalf("OCI-Subject = %q", resp.Header.Get("OCI-Subject"))
		}
	}
	type index struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Digest       string            `json:"digest"`
			ArtifactType string            `json:"artifactType"`
			Annotations  map[string]string `json:"annotations"`
			Size         int64             `json:"size"`
		} `json:"manifests"`
	}
	resp, body := tr.do(http.MethodGet, "/v2/"+name+"/referrers/"+string(img.digest), nil, alice)
	tr.expect(resp, body, http.StatusOK)
	var idx index
	_ = json.Unmarshal(body, &idx)
	if resp.Header.Get("Content-Type") != "application/vnd.oci.image.index.v1+json" || len(idx.Manifests) != 2 {
		t.Fatalf("referrers = %v %s", resp.Header, body)
	}
	resp, body = tr.do(http.MethodGet, "/v2/"+name+"/referrers/"+string(img.digest)+"?artifactType=application/spdx%2Bjson", nil, alice)
	tr.expect(resp, body, http.StatusOK)
	idx = index{}
	_ = json.Unmarshal(body, &idx)
	if resp.Header.Get("OCI-Filters-Applied") != "artifactType" || len(idx.Manifests) != 1 ||
		idx.Manifests[0].Digest != string(core.SHA256Of(sbom)) || idx.Manifests[0].Size != int64(len(sbom)) {
		t.Fatalf("filtered = %s", body)
	}
	// Unknown subject: 200 with an empty list; bad digest: 400.
	resp, body = tr.do(http.MethodGet, "/v2/"+name+"/referrers/"+string(core.SHA256Of([]byte("none"))), nil, alice)
	tr.expect(resp, body, http.StatusOK)
	if !strings.Contains(string(body), `"manifests":[]`) {
		t.Fatalf("empty referrers = %s", body)
	}
	resp, body = tr.do(http.MethodGet, "/v2/"+name+"/referrers/sha256:zz", nil, alice)
	tr.expect(resp, body, http.StatusBadRequest)

	// Deleting the signature removes it from the referrers list.
	resp, body = tr.do(http.MethodDelete, "/v2/"+name+"/manifests/"+string(core.SHA256Of(sig)), nil, alice)
	tr.expect(resp, body, http.StatusAccepted)
	resp, body = tr.do(http.MethodGet, "/v2/"+name+"/referrers/"+string(img.digest), nil, alice)
	tr.expect(resp, body, http.StatusOK)
	idx = index{}
	_ = json.Unmarshal(body, &idx)
	if len(idx.Manifests) != 1 {
		t.Fatalf("after delete = %s", body)
	}
}

func TestCatalogRBAC(t *testing.T) {
	tr := newTestReg(t)
	tr.pushImage("docker-hosted/app", "v1", newImage("a"), alice)
	tr.pushImage("docker-hosted/team/svc", "v1", newImage("b"), alice)
	tr.pushImage("docker-public/web", "v1", newImage("c"), alice)

	catalog := func(opts ...reqOpt) []string {
		resp, body := tr.do(http.MethodGet, "/v2/_catalog", nil, opts...)
		tr.expect(resp, body, http.StatusOK)
		var c struct {
			Repositories []string `json:"repositories"`
		}
		_ = json.Unmarshal(body, &c)
		return c.Repositories
	}
	if got := catalog(alice); !slices.Equal(got, []string{"docker-hosted/app", "docker-hosted/team/svc", "docker-public/web"}) {
		t.Fatalf("alice catalog = %v", got)
	}
	if got := catalog(basic("bob", "secret")); !slices.Equal(got, []string{"docker-hosted/app", "docker-hosted/team/svc"}) {
		t.Fatalf("bob catalog = %v", got)
	}
	if got := catalog(); !slices.Equal(got, []string{"docker-public/web"}) {
		t.Fatalf("anonymous catalog = %v", got)
	}
	resp, body := tr.do(http.MethodGet, "/v2/_catalog?n=1", nil, alice)
	tr.expect(resp, body, http.StatusOK)
	if resp.Header.Get("Link") == "" {
		t.Fatal("no Link header")
	}
	// A repository-scoped token does not grant the catalog; a catalog token does.
	tok := tr.token("repository:docker-hosted/app:pull", alice)
	resp, body = tr.do(http.MethodGet, "/v2/_catalog", nil, bearer(tok))
	tr.expect(resp, body, http.StatusUnauthorized)
	ctok := tr.token("registry:catalog:*", alice)
	if got := catalog(bearer(ctok)); len(got) != 3 {
		t.Fatalf("catalog via token = %v", got)
	}
}

func TestGroupReadAndWrite(t *testing.T) {
	tr := newTestReg(t)
	tr.env.AddRepo(t, &core.Repository{Name: "docker-team", Format: "docker", Type: core.RepoHosted})
	tr.env.AddRepo(t, &core.Repository{Name: "docker-all", Format: "docker", Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"docker-team", "docker-hosted"}, WriteMember: "docker-hosted"}}})
	tr.env.Auth.AddUser("carol", "secret")
	tr.env.AuthZ.Grant("carol", "repo:docker-all:read", "repo:docker-all:write", "repo:docker-hosted:write", "repo:docker-hosted:read")

	team := newImage("team")
	tr.pushImage("docker-team/app", "v1", team, alice)
	tr.pushImage("docker-hosted/app", "v1", newImage("hosted"), alice)
	tr.pushImage("docker-hosted/app", "v0", newImage("old"), alice)

	// First member wins.
	resp, body := tr.do(http.MethodGet, "/v2/docker-all/app/manifests/v1", nil, basic("carol", "secret"))
	tr.expect(resp, body, http.StatusOK)
	if string(body) != string(team.manifest) {
		t.Fatal("group did not resolve members in order")
	}
	resp, body = tr.do(http.MethodGet, "/v2/docker-all/app/blobs/"+string(core.SHA256Of(team.layer)), nil, basic("carol", "secret"))
	tr.expect(resp, body, http.StatusOK)
	resp, body = tr.do(http.MethodGet, "/v2/docker-all/app/tags/list", nil, basic("carol", "secret"))
	tr.expect(resp, body, http.StatusOK)
	if !strings.Contains(string(body), `"tags":["v0","v1"]`) {
		t.Fatalf("group tags = %s", body)
	}

	// Push through the group lands in the write member, with a group-scoped token.
	tok := tr.token("repository:docker-all/new:pull,push", basic("carol", "secret"))
	img := newImage("via-group")
	tr.pushImage("docker-all/new", "v1", img, bearer(tok))
	if _, err := tr.env.Components.GetAsset(t.Context(), mustRepo(t, tr, "docker-hosted").ID, "new/manifests/v1"); err != nil {
		t.Fatalf("not in write member: %v", err)
	}

	// A group without a write member is read-only.
	tr.env.AddRepo(t, &core.Repository{Name: "docker-ro", Format: "docker", Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"docker-team"}}}})
	resp, body = tr.do(http.MethodPost, "/v2/docker-ro/app/blobs/uploads/", nil, alice)
	tr.expect(resp, body, http.StatusMethodNotAllowed)
}

func TestValidateAndCoordinates(t *testing.T) {
	f := docker.New()
	bad := &core.Repository{Name: "g", Format: "docker", Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"a"}, WriteMember: "b"}}}
	if err := f.Validate(bad); err == nil {
		t.Fatal("write member outside members accepted")
	}
	if err := f.Validate(&core.Repository{Name: "x", Format: "npm"}); err == nil {
		t.Fatal("foreign format accepted")
	}
	tests := []struct {
		path string
		want core.Coordinates
	}{
		{"team/app/manifests/v1", core.Coordinates{Format: "docker", Namespace: "team", Name: "app", Version: "v1", Extra: map[string]string{"type": "tag"}}},
		{"app/blobs/sha256:abc", core.Coordinates{Format: "docker", Name: "app", Version: "sha256:abc", Extra: map[string]string{"type": "blob"}}},
		{"app/manifests/sha256:abc", core.Coordinates{Format: "docker", Name: "app", Version: "sha256:abc", Extra: map[string]string{"type": "manifest"}}},
	}
	for _, tc := range tests {
		got := f.Coordinates(&core.Asset{Path: tc.path})
		if got.Namespace != tc.want.Namespace || got.Name != tc.want.Name || got.Version != tc.want.Version || got.Extra["type"] != tc.want.Extra["type"] {
			t.Errorf("Coordinates(%q) = %+v", tc.path, got)
		}
	}
}

func mustRepo(t *testing.T, tr *testReg, name string) *core.Repository {
	t.Helper()
	r, err := tr.env.Repos.Get(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
