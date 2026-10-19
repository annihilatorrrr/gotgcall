package utils

import (
	"testing"
	"time"
)

func TestDispatcherNoDropInOrder(t *testing.T) {
	d := NewDispatcher(4, nil)
	release := make(chan struct{})
	var got []int
	d.Submit(func() { <-release }) // slow callback: everything below queues behind it
	const n = 5000                 // far beyond the old 256-slot drop-oldest channel
	for i := range n {
		d.Submit(func() { got = append(got, i) })
	}
	close(release)
	d.Close() // drains pending events before returning
	if len(got) != n {
		t.Fatalf("ran %d of %d events", len(got), n)
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("event %d ran as %d: order broken", i, v)
		}
	}
}

func TestDispatcherSurvivesPanicAndIgnoresAfterClose(t *testing.T) {
	d := NewDispatcher(0, nil)
	ran := make(chan struct{})
	d.Submit(func() { panic("boom") })
	d.Submit(func() { close(ran) })
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("event after a panicking callback never ran")
	}
	d.Close()
	d.Close()
	d.Submit(func() { t.Error("ran after Close") })
}
