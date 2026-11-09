package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/api/apitest"
	"github.com/ismoilovdevml/hangar/internal/api/catalog"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/service/cleanup"
	"github.com/ismoilovdevml/hangar/internal/service/component"
	"github.com/ismoilovdevml/hangar/internal/tasks"
)

// TestCleanupAndTasksAPI drives the cleanup policy, task and blob store
// administration endpoints against PostgreSQL.
func TestCleanupAndTasksAPI(t *testing.T) {
	pool := apitest.MigratedPool(t)
	s := apitest.Seeder{T: t, Pool: pool}
	f := newFixture(t)
	f.env.AuthZ.Grant(tasks.SystemActor, "repo:*:admin")
	f.env.AuthZ.Grant("bob", "system:tasks:read", "system:cleanup:read")
	ctx := context.Background()

	store := s.Store("default", "fs", nil)
	spare := s.Store("spare", "fs", nil)
	repoID := s.Repo("raw-hosted", "raw", "hosted", store)
	f.env.Repos.Put(&core.Repository{ID: repoID, Name: "raw-hosted", Format: "raw", Type: core.RepoHosted, Online: true, BlobStore: "default"})
	old := time.Now().AddDate(0, 0, -40)
	for i, v := range []string{"1.0", "1.1"} {
		id := s.Component(repoID, "", "tool", v, old.Add(time.Duration(i)*time.Hour), 0, "ci")
		d := s.Blob(store, "tool-"+v, 500, old)
		s.Asset(repoID, id, "tool/"+v+"/tool.bin", d, 500)
	}

	comps := component.New(pool, component.Options{AuthZ: f.env.AuthZ, Audit: f.env.Audit, Events: f.env.Events, FlushInterval: time.Hour})
	t.Cleanup(func() { _ = comps.Close() })
	f.env.Jobs.RunInline = true
	taskSvc := tasks.New(pool, tasks.Options{Jobs: f.env.Jobs, Events: f.env.Events, Audit: f.env.Audit, Heartbeat: 20 * time.Millisecond})
	t.Cleanup(func() { taskSvc.Close(ctx) })
	cleanSvc := cleanup.New(pool, cleanup.Options{Repos: f.env.Repos, Components: comps, Tasks: taskSvc, Audit: f.env.Audit})
	taskSvc.Register(tasks.TypeCleanup, cleanSvc.Executor(), tasks.TypeOptions{})
	taskSvc.Register("noop", tasks.Func(func(context.Context, *tasks.Exec) error { return nil }), tasks.TypeOptions{Creatable: true})
	if err := taskSvc.Handle(); err != nil {
		t.Fatal(err)
	}
	if err := taskSvc.EnsureBuiltins(ctx, []tasks.Builtin{{Name: "house-keeping", Type: "noop", Schedule: "@daily"}}); err != nil {
		t.Fatal(err)
	}
	f.api.Cleanup, f.api.Tasks = cleanSvc, taskSvc
	f.api.Catalog = catalog.New(pool, catalog.Options{ConfiguredStores: []string{"default"}})
	f.api.Deps.BaseURL = "https://hangar.example.com/"

	// System status exposes the client-facing URLs.
	var st struct {
		BaseURL      string `json:"base_url"`
		RegistryHost string `json:"registry_host"`
	}
	_ = json.Unmarshal(f.expect("", "GET", "/api/v1/system/status", nil, 200), &st)
	if st.BaseURL != "https://hangar.example.com" || st.RegistryHost != "hangar.example.com" {
		t.Errorf("status urls = %+v", st)
	}

	// The repository totals before the cleanup run.
	var repo repositoryJSON
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/repositories/raw-hosted", nil, 200), &repo)
	if repo.SizeBytes != 1000 || repo.ComponentCount != 2 {
		t.Fatalf("totals before cleanup = %d bytes, %d components", repo.SizeBytes, repo.ComponentCount)
	}

	// Preview -> save -> run.
	in := map[string]any{"name": "old-tools", "criteria": map[string]any{"older_than_days": 30, "keep_newest": 1},
		"repositories": []string{"raw-hosted"}, "schedule": "0 2 * * *"}
	f.expect("bob", "POST", "/api/v1/cleanup-policies/preview", in, 403)
	var prev cleanup.Preview
	_ = json.Unmarshal(f.expect("admin", "POST", "/api/v1/cleanup-policies/preview", in, 200), &prev)
	if prev.ComponentCount != 1 || prev.Bytes != 500 || len(prev.Sample) != 1 || prev.Sample[0].Version != "1.0" ||
		prev.Sample[0].SizeBytes != 500 || prev.PreviewToken == "" {
		t.Fatalf("preview = %+v", prev)
	}
	resp, body := f.do("admin", "POST", "/api/v1/cleanup-policies/preview?format=csv", in)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") ||
		!strings.Contains(string(body), "raw-hosted,raw,,tool,1.0,500,") {
		t.Fatalf("csv = %d %s", resp.StatusCode, body)
	}
	var e Error
	_ = json.Unmarshal(f.expect("admin", "POST", "/api/v1/cleanup-policies", in, 400), &e)
	if e.Code != "preview_required" {
		t.Errorf("save without preview = %+v", e)
	}
	in["preview_token"] = prev.PreviewToken
	var pol cleanup.Policy
	_ = json.Unmarshal(f.expect("admin", "POST", "/api/v1/cleanup-policies", in, 201), &pol)
	if pol.ScheduleText != "Every day at 02:00 UTC" || !pol.Enabled || pol.TaskID == "" {
		t.Fatalf("policy = %+v", pol)
	}
	var plist page[cleanup.Policy]
	_ = json.Unmarshal(f.expect("bob", "GET", "/api/v1/cleanup-policies", nil, 200), &plist)
	if len(plist.Items) != 1 || plist.Items[0].Name != "old-tools" {
		t.Fatalf("policies = %+v", plist)
	}
	f.expect("bob", "POST", "/api/v1/cleanup-policies/old-tools/run", nil, 403)
	var run tasks.Run
	_ = json.Unmarshal(f.expect("admin", "POST", "/api/v1/cleanup-policies/old-tools/run", nil, 202), &run)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/tasks/"+pol.TaskID+"/runs/"+run.ID, nil, 200), &run)
		if !run.Active() || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if run.Status != tasks.StatusSucceeded || run.ComponentsDeleted == nil || *run.ComponentsDeleted != 1 ||
		run.BytesFreed == nil || *run.BytesFreed != 500 || run.Log != nil {
		t.Fatalf("run = %+v", run)
	}
	_ = json.Unmarshal(f.expect("admin", "GET", "/api/v1/repositories/raw-hosted", nil, 200), &repo)
	if repo.SizeBytes != 500 || repo.ComponentCount != 1 {
		t.Errorf("totals after cleanup = %d bytes, %d components", repo.SizeBytes, repo.ComponentCount)
	}
	var runs page[tasks.Run]
	_ = json.Unmarshal(f.expect("bob", "GET", "/api/v1/cleanup-policies/old-tools/runs?limit=5", nil, 200), &runs)
	if len(runs.Items) != 1 || runs.Items[0].ID != run.ID {
		t.Fatalf("runs = %+v", runs)
	}
	var lines tasks.RunLog
	_ = json.Unmarshal(f.expect("bob", "GET", "/api/v1/tasks/"+pol.TaskID+"/runs/"+run.ID+"/log?after=1&limit=10", nil, 200), &lines)
	if len(lines.Items) != 3 || lines.Items[0].Seq != 2 || lines.Items[1].Message != "Deleted raw-hosted/tool@1.0 (500 bytes)" {
		t.Fatalf("log = %+v", lines)
	}
	f.expect("bob", "GET", "/api/v1/tasks/"+pol.TaskID+"/runs/"+run.ID+"/log?limit=0", nil, 400)
	_ = json.Unmarshal(f.expect("admin", "POST", "/api/v1/cleanup-policies/old-tools/stop", nil, 409), &e)
	if e.Code != "not_running" {
		t.Errorf("stop idle = %+v", e)
	}

	// Tasks: the policy's task and the built-in are listed; managed tasks are protected.
	var tl struct {
		Items   []tasks.Task `json:"items"`
		Running []tasks.Run  `json:"running"`
	}
	_ = json.Unmarshal(f.expect("bob", "GET", "/api/v1/tasks", nil, 200), &tl)
	if len(tl.Items) != 2 || tl.Items[0].Name != "house-keeping" || tl.Items[1].Name != "cleanup/old-tools" || tl.Running == nil {
		t.Fatalf("tasks = %+v", tl)
	}
	_ = json.Unmarshal(f.expect("admin", "DELETE", "/api/v1/tasks/"+url.PathEscape("cleanup/old-tools"), nil, 409), &e)
	if e.Code != "managed" {
		t.Errorf("delete policy task = %+v", e)
	}
	f.expect("bob", "POST", "/api/v1/tasks", map[string]any{"name": "extra", "type": "noop"}, 403)
	f.expect("admin", "POST", "/api/v1/tasks", map[string]any{"name": "extra", "type": "noop", "schedule": "@hourly"}, 201)
	f.expect("admin", "PUT", "/api/v1/tasks/extra", map[string]any{"enabled": false}, 200)
	f.expect("admin", "POST", "/api/v1/tasks", map[string]any{"name": "bad", "type": "rescan"}, 400)
	f.expect("admin", "DELETE", "/api/v1/tasks/extra", nil, 204)
	f.expect("admin", "DELETE", "/api/v1/cleanup-policies/old-tools", nil, 204)
	f.expect("admin", "GET", "/api/v1/cleanup-policies/old-tools", nil, 404)

	// Blob stores: quota changes, empty stores can be deleted, used and configured ones cannot.
	var bs blobStoreJSON
	_ = json.Unmarshal(f.expect("admin", "PUT", "/api/v1/blob-stores/default", map[string]any{"soft_quota_bytes": 1 << 30}, 200), &bs)
	if bs.SoftQuotaBytes == nil || *bs.SoftQuotaBytes != 1<<30 {
		t.Fatalf("quota = %+v", bs)
	}
	_ = json.Unmarshal(f.expect("admin", "PUT", "/api/v1/blob-stores/default", map[string]any{"soft_quota_bytes": nil}, 200), &bs)
	if bs.SoftQuotaBytes != nil {
		t.Fatalf("quota not cleared: %+v", bs)
	}
	f.expect("admin", "PUT", "/api/v1/blob-stores/default", map[string]any{"fs": map[string]string{"path": "/elsewhere"}}, 400)
	f.expect("admin", "PUT", "/api/v1/blob-stores/default", map[string]any{"kind": "s3"}, 400)
	f.expect("admin", "PUT", "/api/v1/blob-stores/spare", map[string]any{"fs": map[string]string{"path": "/srv/spare"}}, 200)
	_ = json.Unmarshal(f.expect("admin", "DELETE", "/api/v1/blob-stores/default", nil, 409), &e)
	if e.Code != "in_use" {
		t.Errorf("delete configured store = %+v", e)
	}
	f.expect("bob", "DELETE", "/api/v1/blob-stores/spare", nil, 403)
	f.expect("admin", "DELETE", "/api/v1/blob-stores/spare", nil, 204)
	f.expect("admin", "DELETE", "/api/v1/blob-stores/spare", nil, 404)
	_ = spare
	outcomes := func(action string) string {
		var out []string
		for _, ev := range f.env.Audit.Events(action) {
			out = append(out, ev.Actor+"/"+ev.Outcome)
		}
		return strings.Join(out, ",")
	}
	if got := outcomes("blobstore.update"); got != "admin/success,admin/success,admin/success" {
		t.Errorf("blobstore.update audit = %s", got)
	}
	if got := outcomes("blobstore.delete"); got != "bob/denied,admin/success" {
		t.Errorf("blobstore.delete audit = %s", got)
	}
}
