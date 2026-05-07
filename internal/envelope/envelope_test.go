package envelope

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAutonomousSafeRejectsByDefault(t *testing.T) {
	t.Parallel()

	e := NewAutonomousSafe()
	d := e.Submit(context.Background(), Proposal{
		Kind:   KindRateLimit,
		Target: "/api/users",
		Value:  150,
		Agent:  "test-agent",
		Reason: "load looks fine",
	})

	if d.Kind != DecisionReject {
		t.Fatalf("autonomous-safe envelope must reject, got %s", d.Kind)
	}
	if d.Stage != "fallback" {
		t.Fatalf("expected stage=fallback, got %q", d.Stage)
	}
	if !strings.Contains(d.Reason, "autonomous-safe") {
		t.Fatalf("decision reason must reference fallback mode, got %q", d.Reason)
	}
	if d.DecidedAt.IsZero() {
		t.Fatal("DecidedAt must be set on every Decision")
	}
}

func TestSubmitHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	e := NewAutonomousSafe()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d := e.Submit(ctx, Proposal{Kind: KindRateLimit, Agent: "x"})
	if d.Kind != DecisionDefer {
		t.Fatalf("cancelled context must produce DecisionDefer, got %s", d.Kind)
	}
	if d.Stage != "intake" {
		t.Fatalf("expected stage=intake on cancellation, got %q", d.Stage)
	}
}

func TestZeroValueEnvelopeIsUsable(t *testing.T) {
	t.Parallel()

	// The pattern's contract: the gateway runs identically with the Envelope
	// present or absent. A zero-value *Envelope must therefore not panic.
	var e Envelope
	d := e.Submit(context.Background(), Proposal{Kind: KindCacheTTL, Agent: "x"})
	if d.Kind != DecisionReject {
		t.Fatalf("zero-value envelope must reject, got %s", d.Kind)
	}
}

func TestDecisionKindString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		k    DecisionKind
		want string
	}{
		{DecisionReject, "reject"},
		{DecisionDefer, "defer"},
		{DecisionApply, "apply"},
		{DecisionKind(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.k.String(); got != c.want {
			t.Errorf("DecisionKind(%d).String() = %q, want %q", c.k, got, c.want)
		}
	}
}

func TestSubmitStampsDecidedAt(t *testing.T) {
	t.Parallel()

	fixed := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	e := &Envelope{now: func() time.Time { return fixed }}

	d := e.Submit(context.Background(), Proposal{Kind: KindRateLimit, Agent: "x"})
	if !d.DecidedAt.Equal(fixed) {
		t.Fatalf("DecidedAt = %v, want %v", d.DecidedAt, fixed)
	}
}
