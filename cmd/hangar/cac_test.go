package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestConfigAsCodeFlagErrors(t *testing.T) {
	t.Setenv("HANGAR_TOKEN", "hgr_flag_test")
	t.Setenv("HANGAR_URL", "")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"files required", []string{"diff"}, "-f is required"},
		{"prune needs an explicit set", []string{"apply", "--prune", "-f", "x.yaml"}, "--prune needs an explicit --apply-set"},
		{"positional arguments", []string{"apply", "-f", "x.yaml", "extra"}, "unexpected arguments"},
		{"server required", []string{"export"}, "--server (or HANGAR_URL) is required"},
		{"unknown kind", []string{"export", "--server", "http://127.0.0.1:1", "--kind", "User"}, `kind "User"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, strings.NewReader(""), &stdout, &stderr); code != 1 {
				t.Fatalf("exit %d, stderr %s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr = %s, want %q", stderr.String(), tc.want)
			}
			if strings.Contains(stdout.String()+stderr.String(), "hgr_flag_test") {
				t.Fatal("token printed")
			}
		})
	}
}
