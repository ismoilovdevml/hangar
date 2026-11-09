package component_test

import (
	"errors"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestPublishReplace(t *testing.T) {
	f := newFixture(t)
	once := f.repo(t, "raw-once", core.RepoHosted, core.WriteAllowOnce)
	deny := f.repo(t, "raw-deny", core.RepoHosted, core.WriteDeny)
	a, b := f.blob(t, "a"), f.blob(t, "b")
	publish := func(r *core.Repository, d core.Digest, replace bool) error {
		_, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{
			Component: comp("g", "lib", "1.0-SNAPSHOT", nil),
			Assets:    []*core.Asset{asset("g/lib/1.0-SNAPSHOT/lib-1.0-SNAPSHOT.jar", d, core.AssetContent)},
			Replace:   replace,
		})
		return err
	}
	if err := publish(once, a, false); err != nil {
		t.Fatal(err)
	}
	if err := publish(once, b, false); !errors.Is(err, core.ErrWriteDenied) {
		t.Fatalf("overwrite without Replace = %v, want ErrWriteDenied", err)
	}
	if err := publish(once, b, true); err != nil {
		t.Fatalf("overwrite with Replace: %v", err)
	}
	got, err := f.svc.GetAsset(f.ctx, once.ID, "g/lib/1.0-SNAPSHOT/lib-1.0-SNAPSHOT.jar")
	if err != nil || got.BlobDigest != b {
		t.Fatalf("replaced asset = %+v, %v", got, err)
	}
	f.wantBlob(t, a, 0, true)
	f.wantBlob(t, b, 1, false)
	if err := publish(deny, a, true); !errors.Is(err, core.ErrWriteDenied) {
		t.Fatalf("Replace under deny = %v, want ErrWriteDenied", err)
	}
}

func TestListComponentsQuarantinedOnly(t *testing.T) {
	f := newFixture(t)
	r := f.repo(t, "raw-h", core.RepoHosted, core.WriteAllow)
	d := f.blob(t, "x")
	var ids []string
	for _, v := range []string{"1", "2", "3"} {
		c, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Component: comp("", "lib", v, nil),
			Assets: []*core.Asset{asset("lib/"+v, d, "")}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE components SET quarantined = true WHERE id = $1`, ids[1]); err != nil {
		t.Fatal(err)
	}
	for _, q := range []core.ComponentQuery{
		{RepositoryID: r.ID, QuarantinedOnly: true},
		{RepositoryID: r.ID, Name: "lib", QuarantinedOnly: true},
	} {
		p, err := f.svc.ListComponents(f.ctx, q)
		if err != nil || len(p.Items) != 1 || p.Items[0].Version != "2" || !p.Items[0].Quarantined {
			t.Fatalf("ListComponents(%+v) = %+v, %v", q, p.Items, err)
		}
	}
	p, err := f.svc.ListComponents(f.ctx, core.ComponentQuery{RepositoryID: r.ID, Name: "other", QuarantinedOnly: true})
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("other name = %+v, %v", p.Items, err)
	}
}
