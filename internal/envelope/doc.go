// Package envelope is the gateway's implementation of the Agentic Envelope
// pattern: a layered safety pipeline through which all AI-agent-driven
// configuration proposals must pass before reaching the deterministic
// control plane.
//
// The full pattern is described in docs/AGENTIC_ENVELOPE.md. This package
// is intentionally a skeleton: only the autonomous-safe default behavior is
// implemented (every proposal is rejected). Stages of the pipeline —
// immutable constraints, bounded deltas, dampening, shadow simulation — are
// scheduled for the agentic phase of the 2030 plan and will be added
// incrementally, behind feature flags, without ever changing the data path.
//
// The package's contract is therefore:
//
//   - The deterministic gateway runs identically whether this package is
//     present or absent.
//   - Calling Envelope.Submit on the default Envelope always returns a
//     reject Decision. This is the autonomous-safe fallback mode and it is
//     the correct behavior until the agent pipeline exists.
//   - No exported function in this package may block on, depend on, or
//     mutate the data path.
package envelope
