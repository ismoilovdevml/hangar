package core_test

import (
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func TestIsUUID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"lower case", "0b6f6b4e-9d1a-4c4b-8f5e-2a8d6b1f0c3a", true},
		{"upper case", "0B6F6B4E-9D1A-4C4B-8F5E-2A8D6B1F0C3A", true},
		{"no hyphens", "0b6f6b4e9d1a4c4b8f5e2a8d6b1f0c3a", false},
		{"non-hex digit", "0b6f6b4e-9d1a-4c4b-8f5e-2a8d6b1f0c3g", false},
		{"hyphen misplaced", "0b6f6b4e9-d1a-4c4b-8f5e-2a8d6b1f0c3a", false},
		{"braced", "{0b6f6b4e-9d1a-4c4b-8f5e-2a8d6b1f0c}", false},
		{"multi-byte rune keeps length 36", "0b6f6b4e-9d1a-4c4b-8f5e-2a8d6b1f0cé", false},
		{"empty", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := core.IsUUID(tc.in); got != tc.want {
				t.Errorf("IsUUID(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
