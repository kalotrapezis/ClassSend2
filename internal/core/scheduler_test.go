package core

import (
	"testing"
	"time"
)

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 5, 13, 12, 30, 0, 0, time.Local)
	cases := []struct {
		name       string
		in         string
		wantHour   int
		wantMin    int
		wantDur    bool
		wantRoll   bool
		wantDurMin int
		err        bool
	}{
		{"absolute future", "13:15", 13, 15, false, false, 0, false},
		{"absolute past rolls over", "11:00", 11, 0, false, true, 0, false},
		{"absolute at same minute rolls over", "12:30", 12, 30, false, true, 0, false},
		{"relative minutes", ":15", 12, 45, true, false, 15, false},
		{"relative 3 default", ":3", 12, 33, true, false, 3, false},
		{"relative trims spaces", "  :5  ", 12, 35, true, false, 5, false},
		{"absolute trims spaces", "  13:15  ", 13, 15, false, false, 0, false},
		{"empty errors", "", 0, 0, false, false, 0, true},
		{"bad relative zero", ":0", 0, 0, false, false, 0, true},
		{"bad relative neg", ":-5", 0, 0, false, false, 0, true},
		{"bad relative junk", ":x", 0, 0, false, false, 0, true},
		{"bad absolute hour", "25:00", 0, 0, false, false, 0, true},
		{"bad absolute min", "12:70", 0, 0, false, false, 0, true},
		{"bad absolute junk", "abc", 0, 0, false, false, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, err := ParseWhen(tc.in, now)
			if tc.err {
				if err == nil {
					t.Fatalf("ParseWhen(%q) expected error, got %+v", tc.in, w)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseWhen(%q) unexpected error: %v", tc.in, err)
			}
			if w.IsDuration != tc.wantDur {
				t.Errorf("IsDuration = %v, want %v", w.IsDuration, tc.wantDur)
			}
			if w.RollOver != tc.wantRoll {
				t.Errorf("RollOver = %v, want %v", w.RollOver, tc.wantRoll)
			}
			if w.DurationMin != tc.wantDurMin {
				t.Errorf("DurationMin = %d, want %d", w.DurationMin, tc.wantDurMin)
			}
			if w.Absolute.Hour() != tc.wantHour || w.Absolute.Minute() != tc.wantMin {
				t.Errorf("Absolute = %v, want H=%d M=%d", w.Absolute, tc.wantHour, tc.wantMin)
			}
		})
	}
}

func TestScheduler_AddCancelList(t *testing.T) {
	s := NewScheduler()
	fired := make(chan string, 4)
	s.Fire = func(j ScheduledJob) { fired <- j.ID }

	// Add a job 1h out (won't fire during the test).
	id1 := s.Add(ScheduledJob{Action: "lock", When: time.Now().Add(time.Hour)})
	id2 := s.Add(ScheduledJob{Action: "shutdown", When: time.Now().Add(2 * time.Hour)})

	if got := s.List(); len(got) != 2 {
		t.Fatalf("List() length = %d, want 2", len(got))
	}
	if _, ok := s.Cancel(id1); !ok {
		t.Fatalf("Cancel(%q) returned false", id1)
	}
	if got := s.List(); len(got) != 1 || got[0].ID != id2 {
		t.Fatalf("after cancel List() = %+v, want [%s]", got, id2)
	}
	if _, ok := s.Cancel("S999"); ok {
		t.Fatalf("Cancel(unknown) returned true")
	}

	select {
	case id := <-fired:
		t.Fatalf("unexpected fire of %s", id)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestScheduler_FiresAndFollowUp(t *testing.T) {
	s := NewScheduler()
	var fireOrder []string
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	done := make(chan struct{})
	s.Fire = func(j ScheduledJob) {
		<-mu
		fireOrder = append(fireOrder, j.Action)
		mu <- struct{}{}
		if j.Action == "unlock" {
			close(done)
		}
	}
	follow := &ScheduledJob{Action: "unlock", When: time.Now().Add(40 * time.Millisecond)}
	s.Add(ScheduledJob{Action: "lock", When: time.Now().Add(10 * time.Millisecond), FollowUp: follow})

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("follow-up never fired; got %v", fireOrder)
	}
	if len(fireOrder) != 2 || fireOrder[0] != "lock" || fireOrder[1] != "unlock" {
		t.Fatalf("fireOrder = %v, want [lock unlock]", fireOrder)
	}
}
