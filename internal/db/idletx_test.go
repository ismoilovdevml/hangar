package db

import (
	"context"
	"net/url"
	"os"
	"testing"
)

func TestIdleInTransactionTimeout(t *testing.T) {
	base := os.Getenv(EnvTestDatabaseURL)
	if base == "" {
		t.Skipf("%s not set; skipping database integration test", EnvTestDatabaseURL)
	}
	withParam := func(v string) string {
		u, err := url.Parse(base)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set(idleInTxParam, v)
		u.RawQuery = q.Encode()
		return u.String()
	}
	for _, tc := range []struct {
		name, dsn, want string
	}{
		{"default", base, "5min"},
		{"set in the url", withParam("90s"), "90s"},
		{"disabled in the url", withParam("0"), "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := poolFor(t, tc.dsn, 2)
			var got string
			if err := pool.QueryRow(context.Background(), "SHOW "+idleInTxParam).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("%s = %q, want %q", idleInTxParam, got, tc.want)
			}
		})
	}
}

// JIT is off on Hangar's connections unless the URL turns it on, and both
// session settings are applied together.
func TestJITOff(t *testing.T) {
	base := os.Getenv(EnvTestDatabaseURL)
	if base == "" {
		t.Skipf("%s not set; skipping database integration test", EnvTestDatabaseURL)
	}
	withParams := func(kv ...string) string {
		u, err := url.Parse(base)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		for i := 0; i < len(kv); i += 2 {
			q.Set(kv[i], kv[i+1])
		}
		u.RawQuery = q.Encode()
		return u.String()
	}
	for _, tc := range []struct {
		name, dsn, wantJIT, wantIdle string
	}{
		{"default", base, "off", "5min"},
		{"jit on in the url", withParams(jitParam, "on"), "on", "5min"},
		{"idle set in the url", withParams(idleInTxParam, "90s"), "off", "90s"},
		{"both set in the url", withParams(jitParam, "on", idleInTxParam, "90s"), "on", "90s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := poolFor(t, tc.dsn, 2)
			var jit, idle string
			if err := pool.QueryRow(context.Background(),
				"SELECT current_setting('"+jitParam+"'), current_setting('"+idleInTxParam+"')").Scan(&jit, &idle); err != nil {
				t.Fatal(err)
			}
			if jit != tc.wantJIT || idle != tc.wantIdle {
				t.Fatalf("jit = %q, %s = %q; want %q, %q", jit, idleInTxParam, idle, tc.wantJIT, tc.wantIdle)
			}
		})
	}
}
