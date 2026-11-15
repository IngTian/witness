package store

import "testing"

func TestGenerationStatus(t *testing.T) {
	s := tempStore(t)
	if cur, com, err := GenerationStatus(s.db, "sess", "lens", 0, 0); err != nil || !cur || com {
		t.Fatalf("empty session, rawHigh 0: (%v,%v,%v), want current, not committed", cur, com, err)
	}
	appendN(t, s, "sess", 3)
	_, high, _ := s.ReadRawSnapshot("sess")
	if cur, _, _ := GenerationStatus(s.db, "sess", "lens", high, 3); !cur {
		t.Fatal("the high-water generation must be current")
	}
	if cur, _, _ := GenerationStatus(s.db, "sess", "lens", high-1, 2); cur {
		t.Fatal("a generation with a later raw row must not be current")
	}
	if err := s.MarkDistilled("sess", "lens", 3); err != nil {
		t.Fatal(err)
	}
	if _, com, _ := GenerationStatus(s.db, "sess", "lens", high, 3); !com {
		t.Fatal("distilled >= total must read as committed")
	}
}
