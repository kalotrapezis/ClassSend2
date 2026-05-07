package tui

import (
	"sort"
	"testing"
)

func TestHostnameLess_NumericSuffix(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		// Plain numeric suffix — natural order.
		{
			in:   []string{"Lab10", "Lab2", "Lab1"},
			want: []string{"Lab1", "Lab2", "Lab10"},
		},
		// Zero-padded numbers should sort with bare ones.
		{
			in:   []string{"Lab2", "Lab01", "Lab10", "Lab1"},
			want: []string{"Lab01", "Lab1", "Lab2", "Lab10"},
		},
		// Different prefixes cluster alphabetically; within prefix sort numerically.
		{
			in:   []string{"PC3", "Lab2", "PC1", "Lab10", "PC2", "Lab1"},
			want: []string{"Lab1", "Lab2", "Lab10", "PC1", "PC2", "PC3"},
		},
		// Same prefix, mix numbered + unnumbered: numbered first.
		{
			in:   []string{"Lab", "Lab1", "Lab2"},
			want: []string{"Lab1", "Lab2", "Lab"},
		},
		// MAC-style names without numeric suffix sort lexicographically.
		{
			in:   []string{"DESKTOP-XYZ", "DESKTOP-ABC"},
			want: []string{"DESKTOP-ABC", "DESKTOP-XYZ"},
		},
		// Real-world mix from the user's classroom.
		{
			in:   []string{"DESKTOP-RAHDSB6", "PC-T8R2", "α-PC", "PC-WMXN", "PC-VSH0", "DESKTOP-GLT926J"},
			want: []string{"DESKTOP-GLT926J", "DESKTOP-RAHDSB6", "PC-T8R2", "PC-VSH0", "PC-WMXN", "α-PC"},
		},
	}
	for _, c := range cases {
		got := append([]string(nil), c.in...)
		sort.SliceStable(got, func(i, j int) bool { return hostnameLess(got[i], got[j]) })
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Errorf("input %v\n  got:  %v\n  want: %v", c.in, got, c.want)
				break
			}
		}
	}
}

func TestSplitHostNum(t *testing.T) {
	cases := []struct {
		s          string
		wantPrefix string
		wantN      int
		wantOK     bool
	}{
		{"Lab1", "Lab", 1, true},
		{"Lab07", "Lab", 7, true},
		{"PC10", "PC", 10, true},
		{"DESKTOP-RAHDSB6", "DESKTOP-RAHDSB", 6, true},
		{"Lab", "Lab", 0, false},
		{"", "", 0, false},
	}
	for _, c := range cases {
		p, n, ok := splitHostNum(c.s)
		if p != c.wantPrefix || n != c.wantN || ok != c.wantOK {
			t.Errorf("splitHostNum(%q) = (%q, %d, %v), want (%q, %d, %v)",
				c.s, p, n, ok, c.wantPrefix, c.wantN, c.wantOK)
		}
	}
}
