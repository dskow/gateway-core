// Package envelope is the gateway's implementation of the Agentic Envelope
// pattern: a layered safety pipeline through which all AI-agent-driven
// configuration proposals must pass before reaching the deterministic
// control plane.
//
// The full pattern is described in docs/AGENTIC_ENVELOPE.md. Stages are
// added incrementally, behind a stable interface, without ever changing
// the data path. The current implementation provides:
//
//   - The Proposal / Decision value types and the Envelope.Submit entry point.
//   - The autonomous-safe default: every unmatched proposal is rejected at
//     stage "fallback" with ErrFallback.
//   - The immutable-constraints stage: an operator-authored ConstraintRegistry
//     that runs before any later stage. A violation short-circuits with stage
//     "constraints" and a structured reason; the rest of the pipeline (bounds,
//     dampener, shadow) is not consulted.
//
// The package's contract is therefore:
//
//   - The deterministic gateway runs identically whether this package is
//     present or absent.
//   - An Envelope built with no options (NewAutonomousSafe or New()) rejects
//     every proposal at the fallback stage — this is the autonomous-safe
//     fallback mode and it is the correct behavior until the full agent
//     pipeline exists.
//   - An Envelope built with WithConstraints rejects malformed or
//     unconstitutional proposals at the constraints stage and otherwise
//     also falls back. Constraints never weaken the autonomous-safe contract;
//     they only produce clearer rejection reasons.
//   - No exported function in this package may block on, depend on, or
//     mutate the data path.
package envelope
