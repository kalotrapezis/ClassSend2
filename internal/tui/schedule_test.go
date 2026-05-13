package tui

import "testing"

func TestSplitToolTimeClause(t *testing.T) {
	cases := []struct {
		in       string
		wantBody string
		wantTime string
		wantOK   bool
	}{
		// Normal cases — body / time split on " | ".
		{"shutdown >* | 13:15", "shutdown >*", "13:15", true},
		{"lock | :15", "lock", ":15", true},
		{"lock >2 | :3", "lock >2", ":3", true},
		// No pipe at all.
		{"lock >2", "lock >2", "", false},
		{"shutdown", "shutdown", "", false},
		// Tolerant of missing spaces around pipe; the time parser handles
		// the error if `:NN` / `HH:MM` parsing fails downstream.
		{"lock>*|:15", "lock>*", ":15", true},
		{"lock|:15", "lock", ":15", true},
		// Pipe with trailing whitespace stays detected (Tab-completion case).
		{"lock >* |", "lock >*", "", true},
	}
	for _, tc := range cases {
		body, ts, ok := splitToolTimeClause(tc.in)
		if body != tc.wantBody || ts != tc.wantTime || ok != tc.wantOK {
			t.Errorf("splitToolTimeClause(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, body, ts, ok, tc.wantBody, tc.wantTime, tc.wantOK)
		}
	}
}

func TestSchedulableActionsExcludesFocusAndShot(t *testing.T) {
	for _, a := range []string{"focus", "fc", "shot", "sh", "cast", "caston", "stop-casting"} {
		if schedulableActions[a] {
			t.Errorf("schedulableActions[%q] = true, want false (per design)", a)
		}
	}
	for _, a := range []string{"lock", "lk", "unlock", "shutdown", "sd", "mute", "launch", "tvon"} {
		if !schedulableActions[a] {
			t.Errorf("schedulableActions[%q] = false, want true", a)
		}
	}
}

func TestDefaultTimeForAction(t *testing.T) {
	if defaultTimeForAction("lock") != ":15" {
		t.Errorf("lock default = %q, want :15", defaultTimeForAction("lock"))
	}
	if defaultTimeForAction("lk") != ":15" {
		t.Errorf("lk default = %q, want :15", defaultTimeForAction("lk"))
	}
	if defaultTimeForAction("shutdown") != ":3" {
		t.Errorf("shutdown default = %q, want :3", defaultTimeForAction("shutdown"))
	}
	if defaultTimeForAction("") != ":3" {
		t.Errorf("empty default = %q, want :3", defaultTimeForAction(""))
	}
}
