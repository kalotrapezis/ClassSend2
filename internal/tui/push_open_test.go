package tui

import "testing"

// Regression for the v0.1.0 bug where Path Notes inserted `--op "google.gr" >*`
// and parsePushOpenCmd's strings.Fields split left the quote characters in
// target. The agent then asked Windows to open a protocol literally named
// `"google.gr"` and got nothing. Quotes around the target must be stripped,
// and quoted paths-with-spaces must survive as a single token.
func TestParsePushOpenCmd_QuoteHandling(t *testing.T) {
	cases := []struct {
		in         string
		wantTarget string
		wantDest   string
		wantOk     bool
	}{
		{`--op google.gr >*`, "google.gr", "*", true},
		{`--op "google.gr" >*`, "google.gr", "*", true},
		{`--op "C:\Path with spaces\f.pdf" >3`, `C:\Path with spaces\f.pdf`, "3", true},
		{`--op https://example.com >7`, "https://example.com", "7", true},
		{`--open "https://example.com" >`, "https://example.com", "*", true},
		{`google.gr --op >*`, "google.gr", "*", true},
		{`"google.gr" --op >5`, "google.gr", "5", true},
		// No ">": local open, not a push.
		{`--op google.gr`, "", "", false},
	}
	for _, tc := range cases {
		gotT, gotD, gotOk := parsePushOpenCmd(tc.in)
		if gotT != tc.wantTarget || gotD != tc.wantDest || gotOk != tc.wantOk {
			t.Errorf("parsePushOpenCmd(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, gotT, gotD, gotOk, tc.wantTarget, tc.wantDest, tc.wantOk)
		}
	}
}
