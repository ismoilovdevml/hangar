package docker_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/docker"
)

// pushIndex pushes an OCI index of the given children under ref.
func (tr *testReg) pushIndex(name, ref string, children ...map[string]any) core.Digest {
	tr.t.Helper()
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": children})
	resp, body := tr.do(http.MethodPut, "/v2/"+name+"/manifests/"+ref, idx, alice, header("Content-Type", "application/vnd.oci.image.index.v1+json"))
	tr.expect(resp, body, http.StatusCreated)
	return core.SHA256Of(idx)
}

func child(d core.Digest, platform map[string]string, annotations map[string]string) map[string]any {
	c := map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": d, "size": 100}
	if platform != nil {
		c["platform"] = platform
	}
	if annotations != nil {
		c["annotations"] = annotations
	}
	return c
}

func TestScanManifests(t *testing.T) {
	tr := newTestReg(t)
	ctx := t.Context()
	const name = "docker-hosted/team/app"
	amd := tr.pushLayers(name, "amd", []byte(`{"os":"linux","architecture":"amd64"}`), []byte("amd layer"))
	arm := tr.pushLayers(name, "arm", []byte(`{"os":"linux","architecture":"arm64"}`), []byte("arm layer"))
	att := tr.pushLayers(name, "att", []byte(`{"os":"unknown"}`), []byte("provenance"))
	idx := tr.pushIndex(name, "1.0",
		child(amd, map[string]string{"os": "linux", "architecture": "amd64"}, nil),
		child(arm, map[string]string{"os": "linux", "architecture": "arm64", "variant": "v8"}, nil),
		child(att, map[string]string{"os": "unknown", "architecture": "unknown"},
			map[string]string{"vnd.docker.reference.type": "attestation-manifest"}))

	got, err := tr.format.ScanManifests(ctx, "docker-hosted", "team/app", string(idx))
	if err != nil {
		t.Fatal(err)
	}
	want := []docker.ScanManifest{{Digest: string(amd), Platform: "linux/amd64"}, {Digest: string(arm), Platform: "linux/arm64/v8"}}
	if !slices.Equal(got, want) {
		t.Fatalf("index manifests = %+v", got)
	}
	got, err = tr.format.ScanManifests(ctx, "docker-hosted", "team/app", string(amd))
	if err != nil || !slices.Equal(got, []docker.ScanManifest{{Digest: string(amd)}}) {
		t.Fatalf("image manifests = %+v, %v", got, err)
	}

	// A Helm chart is no image.
	cfg := []byte(`{"name":"web","version":"0.1.0"}`)
	tr.pushBlob(name, cfg, alice)
	layer := []byte("chart tgz")
	tr.pushBlob(name, layer, alice)
	chart, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.cncf.helm.config.v1+json", "digest": core.SHA256Of(cfg), "size": len(cfg)},
		"layers": []any{map[string]any{"mediaType": "application/vnd.cncf.helm.chart.content.v1.tar+gzip", "digest": core.SHA256Of(layer), "size": len(layer)}}})
	resp, body := tr.do(http.MethodPut, "/v2/"+name+"/manifests/chart", chart, alice, header("Content-Type", "application/vnd.oci.image.manifest.v1+json"))
	tr.expect(resp, body, http.StatusCreated)
	_, err = tr.format.ScanManifests(ctx, "docker-hosted", "team/app", string(core.SHA256Of(chart)))
	if !errors.Is(err, docker.ErrNotImage) || !strings.Contains(err.Error(), "Helm chart") {
		t.Fatalf("chart = %v", err)
	}
	// An index of attestations only has nothing to scan.
	only := tr.pushIndex(name, "att-only", child(att, map[string]string{"os": "unknown", "architecture": "unknown"}, nil))
	if _, err := tr.format.ScanManifests(ctx, "docker-hosted", "team/app", string(only)); !errors.Is(err, docker.ErrNotImage) {
		t.Fatalf("attestation index = %v", err)
	}
	if _, err := tr.format.ScanManifests(ctx, "docker-hosted", "team/app", "sha256:"+strings.Repeat("0", 64)); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown manifest = %v", err)
	}
}

func TestScanTokenPullsOnly(t *testing.T) {
	qr := newQuarantineReg(t)
	tr := qr.testReg
	const name = "docker-hosted/team/app"
	layer := []byte("vulnerable layer")
	d := tr.pushLayers(name, "1.0", []byte(`{"os":"linux"}`), layer)
	qr.set("docker-hosted", "team/app", "1.0", true, true)
	// The scanner may read; the fake authorizer grants by principal name
	// (the real token is narrowed from the built-in Reader role).
	tr.env.AuthZ.Grant(docker.ScannerPrincipal, "repo:*:read", "repo:*:write")
	host := strings.TrimPrefix(tr.srv.URL, "http://")
	tok, err := tr.format.ScanToken("docker-hosted", "team/app", host)
	if err != nil {
		t.Fatal(err)
	}
	scanner := bearer(tok)
	man := "/v2/" + name + "/manifests/" + string(d)
	blob := "/v2/" + name + "/blobs/" + string(core.SHA256Of(layer))

	// Quarantined content stays readable to the scanner, by digest.
	for _, p := range []string{man, blob} {
		resp, body := tr.do(http.MethodGet, p, nil, scanner)
		tr.expect(resp, body, http.StatusOK)
		resp, body = tr.do(http.MethodGet, p, nil, alice)
		tr.expect(resp, body, http.StatusForbidden)
	}
	// Other repositories are out of scope, writes are refused, and the
	// token cannot be exchanged for a new one.
	tr.pushLayers("docker-public/x", "1", []byte(`{"os":"linux","p":1}`), []byte("public layer"))
	resp, body := tr.do(http.MethodGet, "/v2/docker-public/x/manifests/1", nil, scanner)
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("other repository = %d %s", resp.StatusCode, body)
	}
	resp, body = tr.do(http.MethodPost, "/v2/"+name+"/blobs/uploads/", nil, scanner)
	tr.expect(resp, body, http.StatusForbidden)
	resp, body = tr.do(http.MethodGet, "/v2/token?service="+url.QueryEscape(host)+"&scope=repository:"+name+":pull", nil, scanner)
	tr.expect(resp, body, http.StatusUnauthorized)

	// It expires with the registry token lifetime.
	tr.now = tr.now.Add(docker.DefaultTokenTTL + time.Minute)
	resp, body = tr.do(http.MethodGet, man, nil, scanner)
	tr.expect(resp, body, http.StatusUnauthorized)
	if tr.format.TokenTTL() != docker.DefaultTokenTTL {
		t.Fatalf("ttl = %s", tr.format.TokenTTL())
	}
}
