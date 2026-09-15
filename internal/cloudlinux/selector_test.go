package cloudlinux

import (
	"slices"
	"testing"
)

// A string sort puts 8.10 before 8.9, which is the bug this ordering exists
// to not have. CloudLinux has not shipped a minor of ten yet; the first one
// would otherwise appear in the wrong place with nothing to explain why.
func TestSortVersionsDescending(t *testing.T) {
	got := []string{"7.4", "8.10", "8.9", "5.6", "10.0", "8.1"}
	want := []string{"10.0", "8.10", "8.9", "8.1", "7.4", "5.6"}
	sortVersionsDescending(got)
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestVersionParts(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want [2]int
	}{
		{"8.4", [2]int{8, 4}},
		{"10.0", [2]int{10, 0}},
		{"native", [2]int{0, 0}},
		{"", [2]int{0, 0}},
	} {
		if got := versionParts(tc.in); got != tc.want {
			t.Errorf("versionParts(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
