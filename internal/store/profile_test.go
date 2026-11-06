package store

import "testing"

// ListProfiles underpins the reap: it must enumerate what is on disk, include the unified
// portrait, tolerate a missing dir, and never hand back a name that could escape the dir.
func TestListProfilesEnumeratesWhatIsOnDisk(t *testing.T) {
	s := tempStore(t)
	// No profile dir yet: empty, not an error.
	got, err := s.ListProfiles()
	if err != nil {
		t.Fatalf("a missing profile dir must not be an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %v", got)
	}

	for _, l := range []string{"math", "code", ProfileUnified} {
		if err := s.WriteProfile(l, "x"); err != nil {
			t.Fatal(err)
		}
	}
	got, err = s.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"math": true, "code": true, ProfileUnified: true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected entry %q", g)
		}
	}
	// Sorted, so the reap order is deterministic.
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("not sorted: %v", got)
		}
	}
	// A deleted profile drops out.
	if err := s.DeleteProfile("math"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListProfiles()
	for _, g := range got {
		if g == "math" {
			t.Error("a deleted profile is still enumerated")
		}
	}
}
