package maven_test

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestSnapshotReplaceAtomic redeploys a non-unique SNAPSHOT file under
// allow_once while readers download it: every read gets one of the
// deployed files, never a 404 from a gap between removing the old file and
// storing the new one. Replacing needs only the write privilege.
func TestSnapshotReplaceAtomic(t *testing.T) {
	f := newFixture(t)
	auth := user + ":" + pass // read and write, no delete
	p := "mixed/com/acme/lib/4.0-SNAPSHOT/lib-4.0-SNAPSHOT.jar"
	f.want(f.do(http.MethodPut, p, auth, "build-0"), http.StatusCreated, "first deploy")
	f.want(f.do(http.MethodPut, p, auth, "build-1"), http.StatusCreated, "redeploy without the delete privilege")

	var (
		stop   atomic.Bool
		wg     sync.WaitGroup
		misses atomic.Int64
		reads  atomic.Int64
	)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				code, _ := f.get(p, "reader:r")
				reads.Add(1)
				if code != http.StatusOK {
					misses.Add(1)
				}
			}
		}()
	}
	for i := 2; i < 60; i++ {
		if code, body := f.put(p, auth, "build-"+strconv.Itoa(i)); code != http.StatusCreated {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("redeploy %d = %d %s", i, code, body)
		}
	}
	stop.Store(true)
	wg.Wait()
	if n := misses.Load(); n > 0 {
		t.Fatalf("%d of %d reads during redeploys did not find the file", n, reads.Load())
	}
	if code, body := f.get(p, "reader:r"); code != http.StatusOK || body != "build-59" {
		t.Fatalf("final file = %d %q", code, body)
	}
}

// get and put are goroutine-safe variants of fixture.do.
func (f *fixture) get(repoPath, auth string) (int, string) {
	return f.send(http.MethodGet, repoPath, auth, "")
}

func (f *fixture) put(repoPath, auth, body string) (int, string) {
	return f.send(http.MethodPut, repoPath, auth, body)
}

func (f *fixture) send(method, repoPath, auth, body string) (int, string) {
	req, err := http.NewRequest(method, f.srv.URL+"/repository/"+repoPath, strings.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	u, pw, _ := strings.Cut(auth, ":")
	req.SetBasicAuth(u, pw)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}
