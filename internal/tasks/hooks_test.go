package tasks_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/tasks"
)

// hookExec seals "secret" as "sealed:<v>", keeps it when an update leaves it
// empty, redacts it on output and refuses runs by "mallory".
type hookExec struct{ fakeExec }

func (hookExec) PrepareConfig(ctx context.Context, cfg, prev json.RawMessage) (json.RawMessage, error) {
	if core.PrincipalFrom(ctx).Name == "mallory" {
		return nil, core.ErrForbidden
	}
	var m, p map[string]string
	_ = json.Unmarshal(cfg, &m)
	_ = json.Unmarshal(prev, &p)
	switch {
	case m["secret"] == "" && p != nil:
		m["secret"] = p["secret"]
	case m["secret"] != "":
		m["secret"] = "sealed:" + m["secret"]
	}
	return json.Marshal(m)
}

func (hookExec) AuthorizeRun(ctx context.Context, _ json.RawMessage) error {
	if core.PrincipalFrom(ctx).Name == "mallory" {
		return core.ErrForbidden
	}
	return nil
}

func (hookExec) RedactConfig(cfg json.RawMessage) json.RawMessage {
	var m map[string]string
	if json.Unmarshal(cfg, &m) != nil {
		return cfg
	}
	delete(m, "secret")
	b, _ := json.Marshal(m)
	return b
}

func TestConfigHooks(t *testing.T) {
	e := newEnv(t)
	svc := e.service("n1")
	var seen json.RawMessage
	svc.Register("hooked", hookExec{fakeExec{run: func(_ context.Context, x *tasks.Exec) error {
		seen = x.Task.Config
		return nil
	}}}, tasks.TypeOptions{Creatable: true})
	admin := core.WithPrincipal(context.Background(), &core.Principal{Name: "admin"})
	mallory := core.WithPrincipal(context.Background(), &core.Principal{Name: "mallory"})

	if _, err := svc.Create(mallory, tasks.Input{Name: "h1", Type: "hooked", Config: json.RawMessage(`{"secret":"x"}`)}); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("create as mallory: %v", err)
	}
	created, err := svc.Create(admin, tasks.Input{Name: "h1", Type: "hooked", Config: json.RawMessage(`{"secret":"pw","repo":"r"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(created.Config) != `{"repo":"r"}` {
		t.Fatalf("returned config %s, want the secret removed", created.Config)
	}
	var stored string
	if err := e.pool.QueryRow(context.Background(), `SELECT config->>'secret' FROM tasks WHERE name = 'h1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "sealed:pw" {
		t.Fatalf("stored secret %q", stored)
	}
	// An update without the secret keeps the stored one.
	if _, err := svc.Update(admin, "h1", tasks.Input{Config: json.RawMessage(`{"repo":"r2"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(context.Background(), `SELECT config->>'secret' FROM tasks WHERE name = 'h1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "sealed:pw" {
		t.Fatalf("secret after update %q", stored)
	}
	list, _, err := svc.List(admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range list {
		if task.Name == "h1" && string(task.Config) != `{"repo":"r2"}` {
			t.Fatalf("listed config %s", task.Config)
		}
	}

	if _, err := svc.Trigger(mallory, "h1", "mallory"); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("trigger as mallory: %v", err)
	}
	run, err := svc.Trigger(admin, "h1", "admin")
	if err != nil {
		t.Fatal(err)
	}
	e.dispatch(svc)
	e.waitRun(svc, "h1", run.ID, func(r *tasks.Run) bool { return r.Status == tasks.StatusSucceeded })
	var got map[string]string
	_ = json.Unmarshal(seen, &got)
	if got["secret"] != "sealed:pw" {
		t.Fatalf("executor saw config %s, want the stored secret", seen)
	}
}
