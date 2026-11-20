package archive_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/internal/archive"
)

func TestLimit(t *testing.T) {
	cases := []struct {
		name   string
		src    string
		limit  int64
		want   string
		tooBig bool
	}{
		{name: "shorter than the limit", src: "abc", limit: 10, want: "abc"},
		{name: "exactly the limit", src: "abcdef", limit: 6, want: "abcdef"},
		{name: "over the limit fails instead of truncating", src: "abcdefg", limit: 6, tooBig: true},
		{name: "empty source", src: "", limit: 0, want: ""},
		{name: "zero limit with data", src: "x", limit: 0, tooBig: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := io.ReadAll(archive.Limit(strings.NewReader(tc.src), tc.limit, "test stream"))
			if tc.tooBig {
				if !errors.Is(err, archive.ErrTooLarge) || !errors.Is(err, core.ErrInvalid) {
					t.Fatalf("err = %v, want ErrTooLarge and core.ErrInvalid", err)
				}
				if !strings.Contains(err.Error(), "test stream") {
					t.Errorf("error %q does not name the stream", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(b, []byte(tc.want)) {
				t.Errorf("read %q, want %q", b, tc.want)
			}
		})
	}
}
