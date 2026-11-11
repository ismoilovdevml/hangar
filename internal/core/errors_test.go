package core_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestMessage(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{fmt.Errorf("remote url %q: blocked by outbound address policy: %w", "http://x", core.ErrInvalid),
			`remote url "http://x": blocked by outbound address policy`},
		{fmt.Errorf("wrap: %w", fmt.Errorf("limit: want 1..1000: %w", core.ErrInvalid)), "wrap: limit: want 1..1000"},
		// The sentinel alone keeps its text: there is nothing else to say.
		{core.ErrInvalid, "invalid argument"},
		// Other sentinels finish the sentence and stay.
		{fmt.Errorf("user %q: %w", "bob", core.ErrAlreadyExists), `user "bob": already exists`},
		{fmt.Errorf("repository %q: %w", "npm", core.ErrNotFound), `repository "npm": not found`},
		// Not an ErrInvalid: an unrelated "invalid argument" text is left alone.
		{errors.New("syscall: invalid argument"), "syscall: invalid argument"},
	}
	for _, c := range cases {
		if got := core.Message(c.err); got != c.want {
			t.Errorf("Message(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
