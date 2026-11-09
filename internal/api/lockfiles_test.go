package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"testing"
)

func TestParseLockfileAPI(t *testing.T) {
	f := newFixture(t)
	f.seedRepos()
	lock := `{"lockfileVersion":3,"packages":{"":{"name":"app"},"node_modules/left-pad":{"version":"1.2.0","resolved":"https://registry.example.com/left-pad/-/left-pad-1.2.0.tgz"},"node_modules/lib":{"resolved":"../lib","link":true}}}`
	b := f.expect("admin", http.MethodPost, "/api/v1/lockfiles?repository=npm-proxy&filename=package-lock.json", lock, http.StatusOK)
	var res struct {
		Kind        string   `json:"kind"`
		Coordinates []string `json:"coordinates"`
		Skipped     int      `json:"skipped"`
	}
	_ = json.Unmarshal(b, &res)
	if res.Kind != "package-lock.json" || len(res.Coordinates) != 1 || res.Coordinates[0] != "left-pad@1.2.0" || res.Skipped != 1 {
		t.Fatalf("result %s", b)
	}
	// gzip-compressed bodies (large lockfiles).
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write([]byte(lock))
	_ = w.Close()
	resp, body := f.do("admin", http.MethodPost, "/api/v1/lockfiles?repository=npm-proxy&filename=package-lock.json", gz.String(), "Content-Encoding", "gzip")
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("left-pad@1.2.0")) {
		t.Fatalf("gzip: %d %s", resp.StatusCode, body)
	}
	f.expect("admin", http.MethodPost, "/api/v1/lockfiles?repository=npm-proxy&filename=list.txt", "left-pad\n", http.StatusBadRequest)
	f.expect("admin", http.MethodPost, "/api/v1/lockfiles?repository=nope&filename=list.txt", "x@1\n", http.StatusNotFound)
	f.expect("alice", http.MethodPost, "/api/v1/lockfiles?repository=npm-proxy&filename=list.txt", "x@1\n", http.StatusForbidden)
}
