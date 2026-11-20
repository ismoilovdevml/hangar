package rpm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// craftedRPM is a lead and the 16-byte preamble of a signature header
// claiming il index entries and dl data bytes, followed by nothing.
func craftedRPM(il, dl uint32) []byte {
	b := make([]byte, leadSize, leadSize+16)
	copy(b, leadMagic)
	b[4] = 3 // format version
	b = append(b, headerMagic...)
	b = append(b, 0, 0, 0, 0)
	b = binary.BigEndian.AppendUint32(b, il)
	return binary.BigEndian.AppendUint32(b, dl)
}

// TestHeaderLengthsAreClaims: a few bytes claiming a huge header are
// refused without allocating what they claim.
func TestHeaderLengthsAreClaims(t *testing.T) {
	cases := []struct {
		name    string
		il, dl  uint32
		wantMsg string
	}{
		{name: "largest header accepted, not sent", il: maxIndex, dl: maxHeaderBytes - maxIndex*16, wantMsg: "truncated"},
		{name: "index over the limit", il: maxIndex + 1, dl: 0, wantMsg: "too large"},
		{name: "data over the limit", il: 1, dl: maxHeaderBytes, wantMsg: "too large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := craftedRPM(tc.il, tc.dl)
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, err := readPackage(bytes.NewReader(body))
			runtime.ReadMemStats(&after)
			if !errors.Is(err, core.ErrInvalid) || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("err = %v, want ErrInvalid mentioning %q", err, tc.wantMsg)
			}
			// The old parser allocated the claimed size (up to 256 MiB) up
			// front. Allow generous slack for the runtime and the race
			// detector.
			if got := after.TotalAlloc - before.TotalAlloc; got > 8<<20 {
				t.Errorf("parsing %d bytes allocated %d bytes", len(body), got)
			}
		})
	}
}
