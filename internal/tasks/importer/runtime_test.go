package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/secrets"
	"github.com/ismoilovdevml/hangar/internal/service/repository/pgtest"
	"github.com/ismoilovdevml/hangar/internal/tasks"
)

func newTaskService(t *testing.T, h *hangar, pool *pgxpool.Pool, jobs *coretest.JobQueue, node string) *tasks.Service {
	t.Helper()
	svc := tasks.New(pool, tasks.Options{Jobs: jobs, Node: node, Heartbeat: 20 * time.Millisecond, ProgressEvery: time.Millisecond})
	svc.Register(TypeImport, h.svc.ImportExecutor(), tasks.TypeOptions{Creatable: true})
	svc.Register(TypePrewarm, h.svc.PrewarmExecutor(), tasks.TypeOptions{Creatable: true})
	t.Cleanup(func() { svc.Close(context.Background()) })
	return svc
}

func dispatch(t *testing.T, svc *tasks.Service, jobs *coretest.JobQueue, seen *int) {
	t.Helper()
	all := jobs.Jobs()
	for _, j := range all[*seen:] {
		if j.Type == tasks.JobType {
			if err := svc.HandleJob(context.Background(), j); err != nil {
				t.Fatalf("handle job: %v", err)
			}
		}
	}
	*seen = len(all)
}

func waitRun(t *testing.T, svc *tasks.Service, task, run string, cond func(*tasks.Run) bool) *tasks.Run {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		r, err := svc.GetRun(context.Background(), task, run)
		if err != nil {
			t.Fatal(err)
		}
		if cond(r) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s: status %s, message %q\n%s", run, r.Status, r.Message, deref(r.Log))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func asAdmin(h *hangar) context.Context {
	h.env.AuthZ.Grant("admin", "repo:*:write", "repo:*:read")
	return core.WithPrincipal(context.Background(), &core.Principal{Name: "admin", Kind: core.PrincipalUser})
}

// TestImportResumesAfterInterruption stops a node in the middle of an
// import; another node resumes from the checkpoint without copying the
// finished packages again.
func TestImportResumesAfterInterruption(t *testing.T) {
	h := newHangar(t)
	h.svc.opts.CheckpointEvery = -1
	h.repo(t, "npm-hosted", "npm", core.RepoHosted, "")
	pkgs := map[string][]string{}
	for i := range 6 {
		pkgs[fmt.Sprintf("pkg-%d", i)] = []string{"1.0.0", "1.1.0"}
	}
	src := npmSource(t, pkgs)
	block := make(chan struct{})
	blocked := make(chan struct{}, 1)
	var once sync.Once
	src.hook = func(r *http.Request) {
		if r.URL.Path == "/pkg-3" {
			select {
			case <-block:
			default:
				once.Do(func() { blocked <- struct{}{} })
				select {
				case <-block:
				case <-r.Context().Done():
				}
			}
		}
	}
	jobs := coretest.NewJobQueue()
	seen := 0
	pool := pgtest.Pool(t)
	n1 := newTaskService(t, h, pool, jobs, "n1")
	cfg, _ := json.Marshal(ImportConfig{Repository: "npm-hosted", Source: Source{URL: src.URL()}, Tuning: Tuning{Concurrency: 1, RequestsPerSecond: 1000}})
	if _, err := n1.Create(asAdmin(h), tasks.Input{Name: "move-npm", Type: TypeImport, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	run, err := n1.Trigger(asAdmin(h), "move-npm", "admin")
	if err != nil {
		t.Fatal(err)
	}
	dispatch(t, n1, jobs, &seen)
	select {
	case <-blocked:
	case <-time.After(20 * time.Second):
		t.Fatal("the import never reached pkg-3")
	}
	cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	n1.Close(cctx)
	cancel()
	r := waitRun(t, n1, "move-npm", run.ID, func(r *tasks.Run) bool { return r.Status == tasks.StatusQueued })
	if !strings.Contains(deref(r.Log), "Interrupted") {
		t.Fatalf("log after hand-back:\n%s", deref(r.Log))
	}
	close(block)

	n2 := newTaskService(t, h, pool, jobs, "n2")
	dispatch(t, n2, jobs, &seen)
	r = waitRun(t, n2, "move-npm", run.ID, func(r *tasks.Run) bool { return !r.Active() })
	if r.Status != tasks.StatusSucceeded {
		t.Fatalf("status %s: %s\n%s", r.Status, r.Message, deref(r.Log))
	}
	var res ImportResult
	_ = json.Unmarshal(r.Result, &res)
	if res.Imported != 12 || res.PackagesDone != 6 {
		t.Fatalf("result %+v", res)
	}
	for i := range 3 {
		if n := src.count(fmt.Sprintf("/pkg-%d", i)); n != 1 {
			t.Errorf("pkg-%d packument fetched %d times, want once (resumed from the checkpoint)", i, n)
		}
	}
	if !strings.Contains(deref(r.Log), "Resuming at package 4 of 6 (pkg-3)") {
		t.Fatalf("log:\n%s", deref(r.Log))
	}
}

// TestSourceCredentialsSealed checks that source credentials are sealed at
// rest, never returned, kept on updates and used to authenticate.
func TestSourceCredentialsSealed(t *testing.T) {
	h := newHangar(t)
	key := strings.Repeat("ab", 32)
	ring, err := secrets.NewKeyring(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.svc.opts.Secrets = ring
	h.repo(t, "npm-hosted", "npm", core.RepoHosted, "")
	src := npmSource(t, map[string][]string{"left-pad": {"1.0.0"}})
	src.user, src.password = "reader", "pa55word"
	jobs := coretest.NewJobQueue()
	seen := 0
	pool := pgtest.Pool(t)
	svc := newTaskService(t, h, pool, jobs, "n1")
	in := ImportConfig{Repository: "npm-hosted", Source: Source{URL: src.URL(), Username: "reader", Password: "pa55word"},
		Filters: Filters{Packages: []string{"left-pad"}}}
	cfg, _ := json.Marshal(in)

	// RBAC: a task administrator without write on the repository is refused.
	h.env.AuthZ.Grant("operator", "repo:*:read")
	op := core.WithPrincipal(context.Background(), &core.Principal{Name: "operator", Kind: core.PrincipalUser})
	if _, err := svc.Create(op, tasks.Input{Name: "imp", Type: TypeImport, Config: cfg}); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("create without repository write: %v", err)
	}

	task, err := svc.Create(asAdmin(h), tasks.Input{Name: "imp", Type: TypeImport, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(task.Config), "pa55word") || strings.Contains(string(task.Config), "hgs1.") {
		t.Fatalf("config returned with the secret: %s", task.Config)
	}
	var stored string
	if err := pool.QueryRow(context.Background(), `SELECT config->'source'->>'password' FROM tasks WHERE name = 'imp'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, "hgs1.") || strings.Contains(stored, "pa55word") {
		t.Fatalf("stored password %q is not sealed", stored)
	}
	// Saving the redacted config back keeps the sealed password.
	if _, err := svc.Update(asAdmin(h), "imp", tasks.Input{Config: task.Config}); err != nil {
		t.Fatal(err)
	}
	var after string
	_ = pool.QueryRow(context.Background(), `SELECT config->'source'->>'password' FROM tasks WHERE name = 'imp'`).Scan(&after)
	if after != stored {
		t.Fatalf("password changed on update: %q", after)
	}
	if _, err := svc.Trigger(op, "imp", "operator"); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("run without repository write: %v", err)
	}
	run, err := svc.Trigger(asAdmin(h), "imp", "admin")
	if err != nil {
		t.Fatal(err)
	}
	dispatch(t, svc, jobs, &seen)
	r := waitRun(t, svc, "imp", run.ID, func(r *tasks.Run) bool { return !r.Active() })
	if r.Status != tasks.StatusSucceeded {
		t.Fatalf("run %s: %s\n%s", r.Status, r.Message, deref(r.Log))
	}
	if strings.Contains(deref(r.Log), "pa55word") {
		t.Fatal("the password reached the run log")
	}

	// Without a key, credentials are refused.
	h.svc.opts.Secrets = nil
	if _, err := svc.Create(asAdmin(h), tasks.Input{Name: "imp2", Type: TypeImport, Config: cfg}); !errors.Is(err, core.ErrNotImplemented) {
		t.Fatalf("create without a key: %v", err)
	}
}
