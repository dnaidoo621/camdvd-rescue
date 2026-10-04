package events

import (
	"testing"
	"time"
)

func TestBurstsCoalesceAndNothingIsDropped(t *testing.T) {
	h := NewHub()
	ch, stop := h.Subscribe()
	defer stop()
	for range 1000 {
		h.Publish("job-1", "")
	}
	h.Publish("drives", "")
	h.Publish("notice", "a")
	h.Publish("notice", "b")
	got := map[string]int{}
	timeout := time.After(time.Second)
	for len(got) < 3 || got["notice"] < 2 {
		select {
		case ev := <-ch:
			got[ev.Name]++
		case <-timeout:
			t.Fatalf("got %v", got)
		}
	}
	if got["job-1"] > 2 {
		t.Errorf("job-1 delivered %d times; bursts should coalesce", got["job-1"])
	}
}
