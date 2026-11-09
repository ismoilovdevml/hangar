package raw

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
)

// slowPublish delays every publish, widening the window between an
// upload's conflict check and its write.
type slowPublish struct {
	*coretest.Components
	delay time.Duration
}

func (s slowPublish) Publish(ctx context.Context, repo *core.Repository, req core.PublishRequest) (*core.Component, []*core.Asset, error) {
	time.Sleep(s.delay)
	return s.Components.Publish(ctx, repo, req)
}

// TestTreeConflictConcurrent uploads "p" and "p/child" at the same time:
// exactly one wins, so a path is never both a file and a directory.
func TestTreeConflictConcurrent(t *testing.T) {
	env := coretest.NewEnv(t)
	env.Auth.AddUser("alice", "secret")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:*:write")
	repo := env.AddRepo(t, &core.Repository{Name: "raw-hosted", Format: Name, Type: core.RepoHosted})
	deps := env.Deps()
	deps.Components = slowPublish{Components: env.Components, delay: 20 * time.Millisecond}
	srv := httptest.NewServer(formats.NewHandler(deps, New()))
	t.Cleanup(srv.Close)

	put := func(p string) int {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/repository/raw-hosted/"+p, strings.NewReader("x"))
		req.SetBasicAuth("alice", "secret")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		res.Body.Close()
		return res.StatusCode
	}
	for i := range 10 {
		file, below := fmt.Sprintf("p%d", i), fmt.Sprintf("p%d/child", i)
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for j, p := range []string{file, below} {
			wg.Add(1)
			go func() { defer wg.Done(); codes[j] = put(p) }()
		}
		wg.Wait()
		_, errFile := env.Components.GetAsset(t.Context(), repo.ID, file)
		_, errBelow := env.Components.GetAsset(t.Context(), repo.ID, below)
		if errFile == nil && errBelow == nil {
			t.Fatalf("both %s and %s were stored (statuses %v)", file, below, codes)
		}
		ok := 0
		for _, c := range codes {
			switch c {
			case http.StatusCreated:
				ok++
			case http.StatusConflict:
			default:
				t.Fatalf("unexpected status %d (statuses %v)", c, codes)
			}
		}
		if ok != 1 {
			t.Fatalf("%d uploads succeeded, want 1 (statuses %v)", ok, codes)
		}
	}
}
