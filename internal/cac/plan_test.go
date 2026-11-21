package cac

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeAPI is an in-memory server: collections of objects by name and the
// resource labels, enough for planning, applying and exporting.
type fakeAPI struct {
	mu      sync.Mutex
	objects map[string]map[string]map[string]any // collection -> name -> object
	labels  map[string]map[string]map[string]string
	calls   []string
	fail    map[string]error // "METHOD path" -> error
}

func newFakeAPI() *fakeAPI {
	f := &fakeAPI{objects: map[string]map[string]map[string]any{}, labels: map[string]map[string]map[string]string{},
		fail: map[string]error{}}
	for _, k := range kinds {
		f.objects[k.Collection] = map[string]map[string]any{}
		f.labels[k.LabelKind] = map[string]map[string]string{}
	}
	return f
}

func (f *fakeAPI) put(k, name string, obj map[string]any, l map[string]string) {
	kd := kindByName(k)
	obj = maps.Clone(obj)
	obj["name"] = name
	j, _ := toJSON(obj)
	f.objects[kd.Collection][name] = j.(map[string]any)
	if l != nil {
		f.labels[kd.LabelKind][name] = l
	}
}

func owned(set string, extra ...string) map[string]string {
	l := map[string]string{LabelManagedBy: ManagedByApply, LabelApplySet: set}
	for i := 0; i+1 < len(extra); i += 2 {
		l[extra[i]] = extra[i+1]
	}
	return l
}

func (f *fakeAPI) Do(_ context.Context, method, path string, body, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method+" "+path)
	if err := f.fail[method+" "+path]; err != nil {
		return err
	}
	u, _ := url.Parse(path)
	answer := func(v any) error {
		if out == nil {
			return nil
		}
		b, _ := json.Marshal(v)
		return json.Unmarshal(b, out)
	}
	var b map[string]any
	if body != nil {
		j, _ := toJSON(body)
		b, _ = j.(map[string]any)
	}
	if u.Path == "/resource-labels" && method == http.MethodGet {
		var items []map[string]any
		for name, l := range f.labels[u.Query().Get("kind")] {
			items = append(items, map[string]any{"name": name, "labels": l})
		}
		return answer(map[string]any{"items": items})
	}
	if rest, ok := strings.CutPrefix(u.Path, "/resource-labels/"); ok {
		kind, name, _ := strings.Cut(rest, "/")
		name, _ = url.PathUnescape(name)
		l := map[string]string{}
		for k, v := range b["labels"].(map[string]any) {
			l[k] = v.(string)
		}
		f.labels[kind][name] = l
		return nil
	}
	if u.Path == "/cleanup-policies/preview" {
		return answer(map[string]any{"component_count": 2, "bytes": 2048, "preview_token": "tok"})
	}
	for coll, objs := range f.objects {
		if u.Path == coll {
			switch method {
			case http.MethodGet:
				items := make([]map[string]any, 0, len(objs))
				for _, n := range slices.Sorted(maps.Keys(objs)) {
					items = append(items, objs[n])
				}
				return answer(map[string]any{"items": items})
			case http.MethodPost:
				delete(b, "preview_token")
				objs[b["name"].(string)] = b
				return answer(b)
			}
		}
		if name, ok := strings.CutPrefix(u.Path, coll+"/"); ok {
			name, _ = url.PathUnescape(name)
			if _, exists := objs[name]; !exists {
				return &APIError{Method: method, Path: path, Status: 404, Code: "not_found"}
			}
			switch method {
			case http.MethodPut:
				delete(b, "preview_token")
				objs[name] = b
				return answer(b)
			case http.MethodDelete:
				delete(objs, name)
				return nil
			}
		}
	}
	return fmt.Errorf("fake: no route %s %s", method, path)
}

func docsOf(t *testing.T, yaml string) []*Document {
	t.Helper()
	docs, err := Parse([]byte(yaml), "test.yaml", ".", testResolver(map[string]string{"TOKEN": "tok-secret"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	return docs
}

func doc(kind, name, spec string) string {
	return "---\napiVersion: hangar/v1\nkind: " + kind + "\nmetadata: {name: " + name + "}\nspec: " + spec + "\n"
}

func ids(p *Plan, action Action) []string {
	var out []string
	for _, c := range p.Changes {
		if action == "" || c.Action == action {
			out = append(out, c.ID())
		}
	}
	return out
}

func TestPlanOrdersByDependency(t *testing.T) {
	f := newFakeAPI()
	docs := docsOf(t, doc(KindCleanupPolicy, "old", "{criteria: {older_than_days: 30}}")+
		doc(KindRepository, "outer", "{format: npm, type: group, config: {group: {members: [inner, zz-hosted]}}}")+
		doc(KindRepository, "inner", "{format: npm, type: group, config: {group: {members: [zz-hosted]}}}")+
		doc(KindRepository, "zz-hosted", "{format: npm, type: hosted, config: {hosted: {}}}")+
		doc(KindRole, "ci", "{privileges: []}")+
		doc(KindBlobStore, "store", "{kind: fs, fs: {path: /x}}")+
		doc(KindOutboundProxy, "corp", "{host: p, port: 1, scheme: http}"))
	p, err := MakePlan(context.Background(), f, "srv", docs, Options{ApplySet: "s"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"OutboundProxy/corp", "BlobStore/store", "Role/ci", "Repository/zz-hosted",
		"Repository/inner", "Repository/outer", "CleanupPolicy/old"}
	if got := ids(p, ActionCreate); !slices.Equal(got, want) {
		t.Fatalf("order = %v\nwant %v", got, want)
	}
	var out bytes.Buffer
	p.Render(&out, false)
	if !strings.Contains(out.String(), "would delete 2 versions (2.0 KiB) if it ran now") ||
		!strings.Contains(out.String(), "Plan: 7 to create, 0 to update, 0 to delete, 0 unchanged.") {
		t.Fatalf("plan:\n%s", out.String())
	}

	cyclic := docsOf(t, doc(KindRepository, "a", "{format: npm, type: group, config: {group: {members: [b]}}}")+
		doc(KindRepository, "b", "{format: npm, type: group, config: {group: {members: [a]}}}"))
	if _, err := MakePlan(context.Background(), f, "srv", cyclic, Options{ApplySet: "s"}); err == nil ||
		!strings.Contains(err.Error(), "groups a, b contain each other") {
		t.Fatalf("cycle: %v", err)
	}
}

func TestPlanOwnership(t *testing.T) {
	hosted := map[string]any{"format": "raw", "type": "hosted", "config": map[string]any{"hosted": map[string]any{}}}
	for _, tc := range []struct {
		name   string
		labels map[string]string
		opts   Options
		action Action
		err    string
		line   string
	}{
		{"owned and equal", owned("s"), Options{}, ActionUnchanged, "", ""},
		{"unlabelled is adopted", nil, Options{}, ActionUpdate, "", `labels: adopted into apply set "s"`},
		{"user label changes", owned("s", "team", "a"), Options{}, ActionUpdate, "", `labels.team: "a" -> (unset)`},
		{"owned by another set", owned("other"), Options{}, "", `owned by apply set "other", not "s"`, ""},
		{"taken over", owned("other"), Options{TakeOwnership: true}, ActionUpdate, "", `labels: moved from apply set "other" to "s"`},
		{"marked by hand for another tool", map[string]string{LabelApplySet: "other"}, Options{}, ActionUpdate, "", "adopted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI()
			f.put(KindRepository, "raw", hosted, tc.labels)
			tc.opts.ApplySet = "s"
			p, err := MakePlan(context.Background(), f, "srv", docsOf(t, doc(KindRepository, "raw", "{format: raw, type: hosted}")), tc.opts)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) || !strings.Contains(err.Error(), "test.yaml:") {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			c := p.Changes[0]
			var out bytes.Buffer
			p.Render(&out, true)
			if c.Action != tc.action || (tc.line != "" && !strings.Contains(out.String(), tc.line)) {
				t.Fatalf("action %s, plan:\n%s", c.Action, out.String())
			}
		})
	}
}

func TestPlanRefusesProtectedResources(t *testing.T) {
	f := newFakeAPI()
	f.put(KindRole, "Admin", map[string]any{"builtin": true, "privileges": []any{"system:*"}}, nil)
	f.put(KindOutboundProxy, "config", map[string]any{"source": "file", "host": "p"}, nil)
	for _, d := range []string{doc(KindRole, "Admin", "{privileges: []}"), doc(KindOutboundProxy, "config", "{host: q}")} {
		if _, err := MakePlan(context.Background(), f, "srv", docsOf(t, d), Options{ApplySet: "s"}); err == nil {
			t.Fatalf("%s: planned", d)
		}
	}
}

func TestPlanPrune(t *testing.T) {
	f := newFakeAPI()
	hosted := map[string]any{"format": "raw", "type": "hosted"}
	f.put(KindRepository, "keep", hosted, owned("s"))
	f.put(KindRepository, "gone-member", hosted, owned("s"))
	f.put(KindRepository, "gone-group", map[string]any{"format": "raw", "type": "group",
		"config": map[string]any{"group": map[string]any{"members": []any{"gone-member", "gone-inner"}}}}, owned("s"))
	f.put(KindRepository, "gone-inner", map[string]any{"format": "raw", "type": "group",
		"config": map[string]any{"group": map[string]any{"members": []any{"gone-member"}}}}, owned("s"))
	f.put(KindRepository, "manual", hosted, nil)
	f.put(KindRepository, "other-set", hosted, owned("other"))
	f.put(KindBlobStore, "default", map[string]any{"kind": "fs", "from_config": true}, owned("s"))
	f.put(KindBlobStore, "old-store", map[string]any{"kind": "fs"}, owned("s"))
	f.put(KindCleanupPolicy, "old-policy", map[string]any{}, owned("s"))
	f.put(KindRole, "old-role", map[string]any{}, owned("s"))

	docs := docsOf(t, doc(KindRepository, "keep", "{format: raw, type: hosted}"))
	p, err := MakePlan(context.Background(), f, "srv", docs, Options{ApplySet: "s", Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"CleanupPolicy/old-policy", "Repository/gone-group", "Repository/gone-inner",
		"Repository/gone-member", "Role/old-role", "BlobStore/old-store"}
	if got := ids(p, ActionDelete); !slices.Equal(got, want) {
		t.Fatalf("deletes = %v\nwant %v", got, want)
	}
	if !p.HasChanges() {
		t.Fatal("no changes")
	}
	// Without --prune nothing is deleted.
	p, err = MakePlan(context.Background(), f, "srv", docs, Options{ApplySet: "s"})
	if err != nil || len(ids(p, ActionDelete)) != 0 || p.HasChanges() {
		t.Fatalf("plan without prune: %v %v", ids(p, ""), err)
	}
	// A kind the token may not read is not pruned, with a warning.
	f.fail["GET /roles"] = &APIError{Status: 403, Code: "forbidden"}
	p, err = MakePlan(context.Background(), f, "srv", docs, Options{ApplySet: "s", Prune: true})
	if err != nil || slices.Contains(ids(p, ActionDelete), "Role/old-role") || len(p.Warnings) != 1 {
		t.Fatalf("forbidden kind: %v %v %v", ids(p, ActionDelete), p.Warnings, err)
	}
}

func TestPlanSecrets(t *testing.T) {
	f := newFakeAPI()
	f.put(KindRepository, "npm-proxy", map[string]any{"format": "npm", "type": "proxy", "config": map[string]any{
		"proxy": map[string]any{"remote_url": "https://r", "auth": map[string]any{"kind": "bearer"}}}}, owned("s"))
	f.put(KindOutboundProxy, "corp", map[string]any{"host": "p", "password_configured": false}, owned("s"))
	docs := docsOf(t, doc(KindRepository, "npm-proxy",
		"{format: npm, type: proxy, config: {proxy: {remote_url: 'https://r', auth: {kind: bearer, token: {valueFrom: {env: TOKEN}}}}}}")+
		doc(KindOutboundProxy, "corp", "{host: p, password: literal-pw}"))
	p, err := MakePlan(context.Background(), f, "srv", docs, Options{ApplySet: "s"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	p.Render(&out, true)
	plan := out.String()
	// The secret cannot be compared: unchanged. The proxy gains a password.
	for _, want := range []string{"= Repository/npm-proxy unchanged", "~ OutboundProxy/corp update", "password: (unset) -> (secret)",
		"note: spec.password is written in the file"} {
		if !strings.Contains(plan, want) {
			t.Fatalf("plan lacks %q:\n%s", want, plan)
		}
	}
	if strings.Contains(plan, "tok-secret") || strings.Contains(plan, "literal-pw") {
		t.Fatalf("plan shows a secret:\n%s", plan)
	}
	p, err = MakePlan(context.Background(), f, "srv", docs, Options{ApplySet: "s", UpdateSecrets: true})
	if err != nil || p.Changes[1].Action != ActionUpdate || !p.Changes[1].ResendSecrets {
		t.Fatalf("--update-secrets: %+v %v", p.Changes[1], err)
	}
	var applied bytes.Buffer
	if err := Apply(context.Background(), f, p, &applied); err != nil {
		t.Fatal(err)
	}
	token, _ := getPath(f.objects["/repositories"]["npm-proxy"], Path{"config", "proxy", "auth", "token"})
	if token != "tok-secret" {
		t.Fatalf("token sent = %v", token)
	}
}

func TestApplyStopsDependentStages(t *testing.T) {
	f := newFakeAPI()
	f.put(KindRole, "stale", map[string]any{}, owned("s"))
	f.fail["POST /blob-stores"] = &APIError{Status: 400, Code: "invalid_argument", Message: "fs.path is required",
		Fields: map[string]string{"fs.path": "required"}}
	docs := docsOf(t, doc(KindBlobStore, "store", "{kind: fs}")+
		doc(KindOutboundProxy, "corp", "{host: p}")+
		doc(KindRepository, "raw", "{format: raw, type: hosted, blob_store: store}"))
	p, err := MakePlan(context.Background(), f, "srv", docs, Options{ApplySet: "s", Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = Apply(context.Background(), f, p, &out)
	if err == nil || err.Error() != "1 of 4 changes failed" {
		t.Fatalf("Apply = %v\n%s", err, out.String())
	}
	for _, want := range []string{"OutboundProxy/corp created", "! BlobStore/store create failed: 400 invalid_argument: fs.path is required",
		"fs.path: required", "Repository/raw skipped", "Role/stale skipped"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if _, ok := f.objects["/repositories"]["raw"]; ok {
		t.Fatal("a repository was created on a failed store")
	}
	if _, ok := f.objects["/roles"]["stale"]; !ok {
		t.Fatal("pruned after a failure")
	}
}

func TestApplyLabelsAndPreview(t *testing.T) {
	f := newFakeAPI()
	docs := docsOf(t, doc(KindCleanupPolicy, "old", "{criteria: {older_than_days: 30}}"))
	p, err := MakePlan(context.Background(), f, "srv", docs, Options{ApplySet: "s"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Apply(context.Background(), f, p, &out); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /cleanup-policies/preview?sample=1", "POST /cleanup-policies", "PUT /resource-labels/cleanup_policy/old"}
	var mutations []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "GET ") {
			mutations = append(mutations, c)
		}
	}
	// The plan previews once for its note; apply previews again for the token.
	if !slices.Equal(mutations[1:], want) {
		t.Fatalf("calls = %v", f.calls)
	}
	if !ownedBy(f.labels["cleanup_policy"]["old"], "s") {
		t.Fatalf("labels = %v", f.labels)
	}
	p, err = MakePlan(context.Background(), f, "srv", docs, Options{ApplySet: "s"})
	if err != nil || p.HasChanges() {
		t.Fatalf("second plan: %v %v", ids(p, ""), err)
	}
}

func TestExportWritesReferences(t *testing.T) {
	f := newFakeAPI()
	f.put(KindRepository, "npm-proxy", map[string]any{"format": "npm", "type": "proxy", "id": "x", "status": "online",
		"config": map[string]any{"proxy": map[string]any{"remote_url": "https://u:REDACTED@r/", "auth": map[string]any{"kind": "basic", "username": "u"}},
			"format": map[string]any{"service_index": "feed-key/v3/index.json"}}}, owned("s", "team", "web"))
	f.put(KindRepository, "nuget-plain", map[string]any{"format": "nuget", "type": "proxy",
		"config": map[string]any{"format": map[string]any{"service_index": "v3/index.json"}}}, nil)
	f.put(KindBlobStore, "s3", map[string]any{"kind": "s3", "s3": map[string]any{"bucket": "b", "access_key_id": "AK"}}, nil)
	f.put(KindBlobStore, "default", map[string]any{"kind": "fs", "from_config": true}, nil)
	f.put(KindOutboundProxy, "corp", map[string]any{"host": "p", "password_configured": true, "source": "database"}, nil)
	f.put(KindRole, "Admin", map[string]any{"builtin": true}, nil)
	var out bytes.Buffer
	notes, err := Export(context.Background(), f, ExportOptions{}, &out)
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"#   HANGAR_SECRET_REPOSITORY_NPM_PROXY_AUTH_PASSWORD  (Repository/npm-proxy spec.config.proxy.auth.password)",
		"env: HANGAR_SECRET_REPOSITORY_NPM_PROXY_PROXY_REMOTE_URL",
		"env: HANGAR_SECRET_REPOSITORY_NPM_PROXY_FORMAT_SERVICE_INDEX",
		"service_index: v3/index.json",
		"env: HANGAR_SECRET_BLOB_STORE_S3_S3_SECRET_ACCESS_KEY",
		"env: HANGAR_SECRET_PROXY_CORP_PASSWORD",
		"metadata:\n  name: npm-proxy\n  labels:\n    team: web",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("export lacks %q:\n%s", want, s)
		}
	}
	for _, bad := range []string{"REDACTED", "feed-key", "status:", "id: x", "Admin", "hangar/managed-by", "hangar/apply-set", "from_config"} {
		if strings.Contains(s, bad) {
			t.Fatalf("export contains %q:\n%s", bad, s)
		}
	}
	if len(notes) != 2 {
		t.Fatalf("notes = %v", notes)
	}
	// Every exported document parses back.
	r := testResolver(map[string]string{}, nil)
	r.LookupEnv = func(string) (string, bool) { return "v", true }
	docs, err := Parse(out.Bytes(), "export.yaml", ".", r)
	if err != nil || len(docs) != 4 {
		t.Fatalf("re-parse: %d docs, %v", len(docs), err)
	}

	out.Reset()
	if _, err := Export(context.Background(), f, ExportOptions{ApplySet: "s", Kinds: []string{KindRepository}}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "name: npm-proxy") || strings.Contains(out.String(), "nuget-plain") {
		t.Fatalf("apply-set export:\n%s", out.String())
	}
	if _, err := Export(context.Background(), f, ExportOptions{Kinds: []string{"User"}}, &out); err == nil {
		t.Fatal("unknown kind exported")
	}
}
