package web

import (
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

// When the grace runs out with nobody dialed in, the hub calls back so
// the snapshot moves the agent from starting to disconnected; a
// subscriber dialing in first cancels that.
func TestAgentpodHubGraceExpiryNotifies(t *testing.T) {
	c := clock.NewFakeAt(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	h := newAgentpodHub()
	fired := make(chan string, 4)
	h.SetGraceExpiry(c, func() { fired <- "expired" })

	// The dialed-in pod first: its timer is cancelled, and even if the
	// fire races the cancel, nothing is reported.
	h.MarkStarting("vp", c.Now())
	sub := h.Subscribe("vp")
	defer h.Unsubscribe(sub)
	c.Advance(podStartGrace)

	h.MarkStarting("cos", c.Now())
	c.Advance(podStartGrace - time.Second)
	select {
	case got := <-fired:
		t.Fatalf("fired (%s) before the grace ran out, or for the pod that dialed in", got)
	default:
	}
	c.Advance(time.Second)
	if got := <-fired; got != "expired" {
		t.Fatalf("got %q", got)
	}
	if h.Starting("cos", c.Now()) {
		t.Error("still starting after the grace")
	}
	select {
	case <-fired:
		t.Fatal("fired twice")
	default:
	}
}

// A pod provisioned moments ago is starting, not disconnected, until it
// dials in or podStartGrace passes.
func TestAgentpodHubStarting(t *testing.T) {
	h := newAgentpodHub()
	t0 := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	if h.Starting("cos", t0) {
		t.Fatal("starting before any provision")
	}
	h.MarkStarting("cos", t0)
	if !h.Starting("cos", t0.Add(podStartGrace-time.Second)) {
		t.Fatal("not starting within the grace")
	}
	if h.Starting("cos", t0.Add(podStartGrace)) {
		t.Fatal("still starting after the grace")
	}
	sub := h.Subscribe("cos")
	if h.Starting("cos", t0.Add(time.Second)) {
		t.Fatal("starting with a subscriber")
	}
	h.Unsubscribe(sub)
	// Once it has connected, losing the connection is a disconnection.
	if h.Starting("cos", t0.Add(2*time.Second)) {
		t.Fatal("starting again after a subscriber left")
	}
}
