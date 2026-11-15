package s3

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	ctx := context.Background()

	f := newFakeS3(testBucket)
	steps := newFakeStore(t, f, nil).Check(ctx)
	if len(steps) != 4 {
		t.Fatalf("steps = %+v, want bucket, write, read, delete", steps)
	}
	for i, name := range []string{CheckBucket, CheckWrite, CheckRead, CheckDelete} {
		if steps[i].Name != name || steps[i].Err != nil {
			t.Errorf("step %d = %+v, want %s ok", i, steps[i], name)
		}
	}
	if left := f.keys(testBucket, ""); len(left) != 0 {
		t.Errorf("objects left behind: %v", left)
	}

	// A missing bucket stops at the first step and names the endpoint.
	steps = newFakeStore(t, newFakeS3(testBucket), func(c *Config) { c.Bucket = "no-such-bucket" }).Check(ctx)
	if len(steps) != 1 || steps[0].Err == nil || !strings.Contains(steps[0].Err.Error(), `bucket "no-such-bucket" not found at http://127.0.0.1`) {
		t.Errorf("missing bucket: %+v", steps)
	}

	// Refused writes report the S3 code; nothing is left to delete.
	f = newFakeS3(testBucket)
	f.fail = func(r *http.Request) (int, string) {
		if r.Method == http.MethodPut {
			return http.StatusForbidden, "AccessDenied"
		}
		return 0, ""
	}
	steps = newFakeStore(t, f, nil).Check(ctx)
	if len(steps) != 2 || steps[1].Name != CheckWrite || steps[1].Err == nil || !strings.Contains(steps[1].Err.Error(), "(AccessDenied)") {
		t.Errorf("refused write: %+v", steps)
	}
}
