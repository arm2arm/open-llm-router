package auth

import (
	"slices"
	"testing"
)

func TestGroupNames(t *testing.T) {
	for _, tc := range []struct {
		format string
		in     []string
		want   []string
	}{
		{GroupLeaf, []string{"staff", "admins", "staff"}, []string{"admins", "staff"}},
		{GroupLeaf, []string{"/staff", "/org/students/"}, []string{"staff", "students"}},
		// Leaf policies have the same meaning for claims and sync.
		{GroupLeaf, []string{"/a/staff", "/b/staff", "/admins"}, []string{"admins", "staff"}},
		{GroupLeaf, []string{"staff", "staff", "admins"}, []string{"admins", "staff"}},
		// A bare leaf next to one path is not ambiguous.
		{GroupLeaf, []string{"staff", "/org/staff"}, []string{"staff"}},
		{GroupLeaf, []string{"", "/", " "}, []string{}},
		{GroupPath, []string{"staff", "/org/staff", "/org/staff/"}, []string{"/org/staff", "/staff"}},
		{GroupPath, []string{"", "/"}, []string{}},
		{"", []string{"/x/y"}, []string{"y"}}, // leaf is the default
	} {
		if got := GroupNames(tc.in, tc.format); !slices.Equal(got, tc.want) {
			t.Errorf("%s %v: got %v want %v", tc.format, tc.in, got, tc.want)
		}
	}
}
