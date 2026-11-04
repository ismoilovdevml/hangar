package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunCommands(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, nil, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "hangar ") {
		t.Fatalf("version: %d %q %q", code, out.String(), errb.String())
	}
	out.Reset()
	if code := run([]string{"version", "--json"}, nil, &out, &errb); code != 0 || !strings.Contains(out.String(), `"go_version"`) {
		t.Fatalf("version --json: %d %q", code, out.String())
	}
	if code := run(nil, nil, &out, &errb); code != 2 {
		t.Fatalf("no args = %d", code)
	}
	if code := run([]string{"frobnicate"}, nil, &out, &errb); code != 2 {
		t.Fatalf("unknown = %d", code)
	}
	t.Setenv("HANGAR_CONFIG", "")
	t.Setenv("HANGAR_DATABASE_URL", "")
	errb.Reset()
	if code := run([]string{"serve"}, nil, &out, &errb); code != 1 || !strings.Contains(errb.String(), "database.url") {
		t.Fatalf("serve without db url: %d %q", code, errb.String())
	}
}
