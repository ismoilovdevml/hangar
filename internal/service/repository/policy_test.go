package repository_test

import (
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestDefaultWritePolicy(t *testing.T) {
	f := newFixture(t)
	for format, want := range map[string]core.WritePolicy{
		"raw": core.WriteAllowOnce, "maven": core.WriteAllowOnce, "npm": core.WriteAllowOnce, "docker": core.WriteAllow,
	} {
		r := f.mustCreate(t, hosted(format+"-default", format))
		if got := r.Config.Hosted.WritePolicy; got != want {
			t.Errorf("%s: default write policy = %q, want %q", format, got, want)
		}
		if got := r.WritePolicy(); got != want {
			t.Errorf("%s: effective write policy = %q, want %q", format, got, want)
		}
	}
}

func TestUpdateKeepsStoredWritePolicy(t *testing.T) {
	f := newFixture(t)
	in := hosted("raw-allow", "raw")
	in.Config.Hosted = &core.HostedConfig{WritePolicy: core.WriteAllow}
	f.mustCreate(t, in)

	// An update without a policy (an older script, a partial body) keeps it.
	up := hosted("raw-allow", "raw")
	up.Description = "changed"
	out, err := f.svc.Update(f.ctx, up)
	if err != nil {
		t.Fatal(err)
	}
	if out.Description != "changed" || out.Config.Hosted.WritePolicy != core.WriteAllow {
		t.Fatalf("update without policy: description %q, policy %+v", out.Description, out.Config.Hosted)
	}

	// A repository stored before policies were always written behaves as allow.
	if _, err := f.pool.Exec(t.Context(), `UPDATE repositories SET config = '{}' WHERE name = 'raw-allow'`); err != nil {
		t.Fatal(err)
	}
	out, err = f.svc.Update(f.ctx, hosted("raw-allow", "raw"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Config.Hosted.WritePolicy != core.WriteAllow {
		t.Fatalf("update of a policy-less row = %+v, want allow", out.Config.Hosted)
	}

	// An explicit policy still changes it.
	up = hosted("raw-allow", "raw")
	up.Config.Hosted = &core.HostedConfig{WritePolicy: core.WriteAllowOnce}
	if out, err = f.svc.Update(f.ctx, up); err != nil || out.Config.Hosted.WritePolicy != core.WriteAllowOnce {
		t.Fatalf("explicit policy update = %+v, %v", out, err)
	}
}
