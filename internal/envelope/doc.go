// Package envelope is the gateway's implementation of the Agentic Envelope
// pattern: a layered safety pipeline through which all AI-agent-driven
// configuration proposals must pass before reaching the deterministic
// control plane.
//
// The full pattern is described in docs/AGENTIC_ENVELOPE.md. Stages are
// added incrementally, behind a stable interface, without ever-changing
// the data path. The current implementation provides:
//
//   - The Proposal / Decision value types and the Envelope.Submit entry point.
//   - The autonomous-safe default: every unmatched proposal is rejected at
//     stage "fallback" with ErrFallback.
//   - The immutable-constraints stage: an operator-authored ConstraintRegistry
//     that runs before any later stage. A violation short-circuits with stage
//     "constraints" and a structured reason; the rest of the pipeline (bounds,
//     dampener, shadow) is not consulted.
//   - The bounded-deltas stage: an operator-authored BoundsRegistry of
//     per-Kind absolute value-range bounds. Runs after constraints; a
//     violation short-circuits with stage "bounds" and a structured reason.
//   - The dampener stage: a stateful, per-(Kind, Target) DampenerRegistry
//     enforcing cooldown (minimum interval between successive applied
//     changes) and hysteresis (minimum |new - last applied| difference).
//     Runs after bounds; a cooldown short-circuit produces DecisionDefer
//     with a precise RetryAfter, while a hysteresis short-circuit produces
//     DecisionReject because the agent must observe a larger change before
//     resubmitting.
//   - The shadow-simulator stage: a per-Kind opt-in ShadowRegistry that
//     hands enabled proposals to a Simulator, which replays a window of
//     captured traffic against the proposed configuration in a sandbox
//     and returns a structured ShadowVerdict. Runs after the dampener
//     under a hard per-evaluation timeout. A regression short-circuits
//     with stage "shadow" and DecisionReject; a timeout short-circuits
//     with stage "shadow" and DecisionDefer (RetryAfter = timeout); a
//     simulator-internal error short-circuits with stage "shadow" and
//     DecisionReject so a buggy simulator never silently green-lights
//     a proposal. The package ships a NoopSimulator and a DefaultSLOScorer
//     so the stage has a usable autonomous-safe default before a real
//     replay engine exists.
//
// The package's contract is therefore:
//
//   - The deterministic gateway runs identically whether this package is
//     present or absent.
//   - An Envelope built with no options (NewAutonomousSafe or New()) rejects
//     every proposal at the fallback stage — this is the autonomous-safe
//     fallback mode, and it is the correct behavior until the full agent
//     pipeline exists.
//   - An Envelope built with WithConstraints rejects malformed or
//     unconstitutional proposals at the constraints stage and otherwise
//     also falls back. Constraints never weaken the autonomous-safe contract;
//     they only produce clearer rejection reasons.
//   - An Envelope built with WithBounds rejects out-of-range proposals at
//     the bounds stage and otherwise also falls back. Bounds layer on top
//     of constraints: constraints encode well-formedness rules owned by the
//     gateway codebase, bounds encode environment-specific operating ranges
//     authored by operators per deployment.
//   - An Envelope built with WithDampener defers proposals that arrive
//     inside the cooldown window for their (Kind, Target) and rejects
//     proposals whose value differs from the last applied value by less
//     than the configured hysteresis band. The dampener has no effect
//     until something has been recorded as applied — until the apply path
//     exists in the deterministic core, the dampener is a pass-through
//     for proposals submitted through Submit.
//   - An Envelope built with WithShadow runs the simulator only for
//     opted-in Kinds, under a hard per-evaluation timeout. The data
//     path is unaffected: simulators are read-only, and a missing or
//     not-yet-built simulator (NoopSimulator) is the autonomous-safe
//     default for the stage.
//   - No exported function in this package may block on, depend on, or
//     mutate the data path.
package envelope
