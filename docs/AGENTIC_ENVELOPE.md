# The Agentic Envelope

A design pattern for safely integrating autonomous AI agents into deterministic
control planes — first applied here to an API gateway.

> **Author:** David Skowronski
> **First publication:** 2026-05-07 (this repository)
> **Status:** Pattern documentation. Partially implemented in `internal/envelope/` (autonomous-safe default). Full pipeline scheduled for the agentic phase of the [2030 plan](API_GATEWAY_MAIN_PLAN_2030.md).

This document is the canonical, citable description of the pattern. Other
documents in this repository ([`ARCHITECTURE.md`](ARCHITECTURE.md),
[`API_GATEWAY_MAIN_PLAN_2030.md`](API_GATEWAY_MAIN_PLAN_2030.md)) describe the
gateway-specific instantiation; this document describes the pattern itself.

---

## 1. Intent

Allow autonomous AI agents to mutate the policy of a running, mission-critical
control plane **without** ever putting the control plane in a state that is
unsafe, unstable, or outside operator-defined bounds — and **without** the
control plane becoming dependent on the agents to function.

## 2. Motivation

Agents are useful. They can analyze traffic, propose rate-limit tweaks, suggest
routing changes, generate plugin scaffolding, and surface anomalies that human
operators miss. The temptation is to wire an agent's output directly into the
control plane: let it adjust thresholds, redirect traffic, push config.

Three failure modes follow immediately:

1. **Constraint violation.** The agent disables auth on a route, opens a path
   that should be permanently blocked, or weakens TLS — because nothing in its
   prompt or tools made those rules absolute.
2. **Oscillation.** Two agents (or one agent reacting to its own effects)
   converge on opposing changes. Rate limit jumps from 100 → 200 → 80 → 220 → …
3. **Dependency inversion.** The control plane silently begins to *require* the
   agent: when the agent crashes, hits a token limit, or returns malformed
   output, the data path degrades.

Human-in-the-loop review fixes (1) and (2) but defeats the whole point of
autonomy. The Agentic Envelope is the alternative: a layered, bounded,
out-of-band pipeline that lets agents propose freely, while the control plane
itself remains deterministic and constitutionally safe.

## 3. Applicability

Use this pattern when **all** of the following hold:

- A deterministic control plane already exists and works without AI.
- Operators want agents to *enhance* the control plane (tuning, anomaly
  response, generation), not replace its operators.
- The blast radius of a wrong policy change is large (revenue, safety,
  compliance).
- Agents are non-deterministic and may fail, hallucinate, or get rate-limited.

Do **not** use this pattern when the control plane has no deterministic mode
(e.g., the agent *is* the policy engine), or when the cost of every change is
trivially reversible (e.g., a chat-summary feature). The Envelope is overhead;
it pays back only where unsafe changes are expensive.

## 4. Structure

```
                          ┌────────────────────────────────────────────┐
                          │            Agent Pipeline (advisory)       │
                          │                                            │
                          │  Planner ─► Verifier ─► Safety ─► Observer │
                          │                                  │         │
                          └──────────────────────────────────┼─────────┘
                                                             │ Proposal
                                                             ▼
        ┌────────────────────────────────────────────────────────────────┐
        │                     The Agentic Envelope                       │
        │                                                                │
        │  ┌──────────────────┐                                          │
        │  │ Immutable        │  rejects proposals that violate          │
        │  │ Constraints      │  non-negotiable rules                    │
        │  └────────┬─────────┘                                          │
        │           ▼                                                    │
        │  ┌──────────────────┐                                          │
        │  │ Bounded Deltas   │  rejects proposals exceeding magnitude   │
        │  │                  │  or rate limits per parameter            │
        │  └────────┬─────────┘                                          │
        │           ▼                                                    │
        │  ┌──────────────────┐                                          │
        │  │ Signal Dampening │  defers proposals that would oscillate;  │
        │  │ (hysteresis,     │  enforces cooldown after a recent change │
        │  │  cooldown)       │                                          │
        │  └────────┬─────────┘                                          │
        │           ▼                                                    │
        │  ┌──────────────────┐                                          │
        │  │ Shadow Simulator │  replays recent traffic against the      │
        │  │                  │  proposed config; emits a safety score   │
        │  └────────┬─────────┘                                          │
        │           ▼                                                    │
        │  ┌──────────────────┐                                          │
        │  │ Decision         │  Apply | Reject | Defer                  │
        │  └────────┬─────────┘                                          │
        └───────────┼────────────────────────────────────────────────────┘
                    │ (Apply only)
                    ▼
        ┌────────────────────────────────────────────────────────────────┐
        │             Deterministic Control Plane (always running)       │
        │   routing • auth • rate limits • circuit breakers • logging    │
        └────────────────────────────────────────────────────────────────┘
                    ▲
                    │  Autonomous-Safe Fallback
                    │  (agents offline → freeze last-good config)
                    │
                    └── continues serving traffic with no agent input
```

## 5. Participants

| Participant            | Responsibility |
|------------------------|----------------|
| **Deterministic Core** | Serves traffic. Never blocks on agent output. Has a static, operator-authored configuration that is sufficient on its own. |
| **Proposal**           | An immutable, structured description of a desired policy change. Includes the parameter, the proposed value, the agent that produced it, and a justification. Never executed directly. |
| **Immutable Constraints** | Operator-authored rules that cannot be relaxed by any agent (e.g., "auth is always required on `/api/*`"). Enforced as a hard prefilter before any other Envelope stage runs. |
| **Bounded Deltas**     | Per-parameter limits: maximum change magnitude (e.g., ±20%), maximum changes per time window, allowed value ranges. Rejects proposals that exceed bounds, with a structured reason. |
| **Dampener**           | Stateful filter that prevents oscillation. Applies hysteresis (a proposal must clear a wider band than the last accepted change reversed) and cooldown (no new change to parameter X within `T` of the last change to X). |
| **Shadow Simulator**   | Replays a window of recent real traffic against the *proposed* configuration in a sandboxed copy of the control plane. Emits an SLO-scored verdict: would the proposal have caused regressions in latency, error rate, or cost? |
| **Decision**           | The terminal verdict: `Apply`, `Reject(reason)`, or `Defer(retry-after)`. Logged with full lineage. |
| **Autonomous-Safe Fallback** | The mode the system enters when agents fail, hit rate limits, or are explicitly disabled. Freezes the last-known-good configuration. Rejects all incoming proposals. The control plane keeps serving normally. |
| **Agent Pipeline** *(advisory, optional)* | The producers of proposals. The pattern does not prescribe their internals; it requires only that all proposals enter the Envelope through a single, ordered intake. |

## 6. Collaborations

1. An agent (or an agent pipeline of any composition) emits a `Proposal`.
2. The Envelope evaluates the proposal **strictly in the order**: Constraints → Bounds → Dampening → Shadow → Decision. Earlier stages can short-circuit later stages.
3. A rejection or deferral is returned to the agent with a structured reason. Agents may learn from these but have no path to override them.
4. Only on `Apply` is the proposal handed to the deterministic control plane's hot-reload mechanism.
5. The control plane's data path **never observes** the agent or the Envelope. It reads only the validated configuration that has already been applied.
6. If the agent pipeline becomes unavailable (timeout, error budget exhausted, operator disable), the Envelope enters fallback. The control plane is unaffected.

## 7. Consequences

**Benefits.**

- **Agents are safe by construction, not by review.** Even a buggy or
  adversarially prompted agent cannot violate immutable constraints.
- **The control plane has no AI dependency.** It runs identically with the
  Envelope online or offline, by design. The fallback path is the *normal*
  path with one feature disabled.
- **Audit lineage.** Every change has a structured record: which agent
  proposed it, which Envelope stage approved it, which traffic the shadow
  simulator replayed, which SLO score it received.
- **Operators retain authority.** Constraints and bounds are operator-owned,
  not agent-owned. Tightening them never requires a model change.

**Costs.**

- **Implementation surface.** Each of the six layers is a real subsystem with
  its own correctness, performance, and observability requirements.
- **Latency between proposal and effect.** Shadow simulation in particular
  takes seconds-to-minutes; the Envelope is unsuitable when a change must
  apply in milliseconds.
- **Operator burden.** Someone has to author the immutable constraints and
  the per-parameter bounds. Done badly, this either over-constrains agents
  into uselessness or under-constrains them into the failure modes above.
- **Two configurations to reason about.** The static operator-authored config
  and the mutated agent-driven overlay must compose correctly under
  hot-reload. This is non-trivial.

## 8. Implementation Notes

### Stage ordering is load-bearing

Constraints must run before bounds, and bounds before shadow simulation. Each
later stage is more expensive than the earlier one; cheap rejections must
short-circuit. More importantly, a proposal that violates an immutable
constraint must never reach the shadow simulator — the simulator is sandboxed
but *not* infinitely sandboxed, and an unconstitutional proposal in flight is
already a bug.

### Dampening is per-parameter, not global

A 100ms cooldown on "any change" is useless: it lets agents alternate which
parameter they tweak. Cooldown must be keyed by the parameter being changed.
Hysteresis bands must likewise be parameter-specific, sized to the noise floor
of that parameter's natural variation.

### Shadow simulation is read-only and time-bounded

The shadow simulator replays a captured window of real traffic against the
proposed configuration. It must not write to any external system, must not
emit metrics that mix with production metrics, and must terminate within a
hard timeout. A proposal that times out in shadow is treated as a deferral,
not a rejection — the agent may resubmit when load drops.

### The fallback mode is not an exception path

It is the *primary* mode of the deterministic control plane. The agent
pipeline and the Envelope are an optional wrapper. This inversion is what
makes the pattern safe: the system is correct when AI is offline by default,
not as a degraded mode.

### Constraints belong in code review, not in prompts

Immutable constraints are operator-authored, version-controlled, and reviewed
like any other code. They are not part of the agent's system prompt and
cannot be modified by agent output. A constraint that lives only in an LLM
prompt is not immutable.

## 9. Implementation Status (this repository)

| Component | Status | Location |
|-----------|--------|----------|
| Deterministic control plane | **Built** — JWT auth, rate limiting, reverse proxy, circuit breakers, hot-reload, TLS, structured errors, admin API. | `internal/{auth,ratelimit,proxy,circuitbreaker,config,tlsutil,admin}` |
| Composable circuit breakers (FailureRate / Adaptive / Timeout / Bulkhead) | **Built** — concrete instance of "operator-authored bounded behavior under load." | `internal/circuitbreaker/` |
| Autonomous-safe fallback as default behavior | **Built** — the gateway runs with no agents at all today; the agent path is purely additive. | (entire codebase) |
| `internal/envelope/` package skeleton | **Built** — `Envelope` type, `Proposal` and `Decision` types, no-op `ShadowRunner`, default policy that rejects all proposals (the autonomous-safe default). | `internal/envelope/` |
| Immutable constraint registry | **Built** — `Constraint` interface, `ConstraintRegistry`, `DefaultConstraints()` (well-formedness baseline), wired as the first pipeline stage via `WithConstraints`. Violations short-circuit at stage `"constraints"` with a structured `*ConstraintViolation`; passing proposals fall through to fallback until later stages exist. | `internal/envelope/constraints.go` |
| Bounded delta enforcement | **Built (value-range)** — per-Kind absolute bounds (`IntBound`, `FloatBound`, `DurationBound`) registered in `BoundsRegistry`, wired as the second pipeline stage via `WithBounds`. Out-of-range proposals short-circuit at stage `"bounds"` with a structured `*BoundsViolation`. Magnitude bounds (e.g., ±20% from current value) and per-window rate bounds remain designed but not built. | `internal/envelope/bounds.go` |
| Dampener (hysteresis + cooldown) | **Designed**, not built. | — |
| Shadow simulator (traffic replay) | **Designed**, not built. | — |
| Multi-agent pipeline (Planner / Verifier / Safety / Observer) | **Designed**, not built. | — |

The package is intentionally an autonomous-safe default: until the
remaining stages are implemented, every proposal that does not violate a
constraint or bound still falls through to a fallback rejection. This
matches the pattern's central claim — the deterministic core works
without agents, and agents are added incrementally without ever putting
the data path at risk. The constraints stage gives clearer rejection
reasons for malformed or unconstitutional proposals (e.g., a non-positive
rate-limit value is rejected at stage `constraints` with reason
`rate_limit.positive: non_positive_value`); the bounds stage rejects
proposals that exceed operator-authored value-range limits at stage
`bounds` with a reason like `bounds(rate_limit): above_maximum`. Neither
stage weakens the autonomous-safe contract, because nothing in the
deterministic core ever observes the rejection.

## 10. Related Work

The Agentic Envelope is a synthesis. Each of its layers has an established
ancestor; the pattern's contribution is the specific composition and the
inversion that makes the core AI-independent.

- **Constitutional AI** (Bai et al., Anthropic, 2022, [arxiv 2212.08073](https://arxiv.org/abs/2212.08073))
  — a *training-time* technique using reinforcement learning from AI feedback
  (RLAIF) over a list of natural-language principles to fine-tune a model's
  behavior. The Envelope's Immutable Constraints are *conceptually* related —
  both express inviolable rules separate from the model's weights — but
  *mechanically* unrelated: CAI shapes a model during training; the Envelope
  enforces operator-authored code-level rules at inference time, with no
  model fine-tuning. Cited here as conceptual ancestry only.
- **Hystrix / resilience4j / Polly** — composable resilience primitives
  (circuit breakers, bulkheads, timeouts). The Envelope reuses the
  composition pattern; it does not reinvent the breakers themselves.
- **Twitter Diffy** (2015), **GitHub Scientist** — shadow / dual-run
  validation of new code paths against captured production traffic. The
  Envelope's Shadow Simulator is this technique applied to *agent-proposed
  configuration* rather than *human-written code*.
- **Kayenta / canary analysis** — automated SLO regression detection for
  deployment validation. The Envelope's shadow scoring is closely related;
  the difference is that canaries run against live traffic while the
  Envelope's simulator runs against captured replay.
- **Control theory** — hysteresis, low-pass filters, and dampening are
  textbook responses to oscillation in feedback systems. The Envelope's
  Dampener is a deliberately conservative application of these ideas to a
  domain (agent-driven config) that historically has not used them.
- **Byzantine fault tolerance / consensus protocols** — the multi-agent
  pipeline's requirement that all four agents agree before a proposal is
  emitted resembles consensus. It is *not* a Byzantine protocol; it is a
  linear pipeline where each agent vetoes proposals that fail its check.
- **GitOps and declarative config** — the requirement that agent-driven
  changes flow through the same hot-reload mechanism as operator-authored
  changes. The Envelope is a generalization: agents are one more proposal
  source, with stricter gating than a human pull request.
- **AI control plane proposals** — various 2024–2026 designs for "AI-native"
  infrastructure. The Envelope differs in its inversion: most such designs
  make the AI agent the primary controller with safety as a feature; the
  Envelope makes the deterministic core the primary controller with AI as
  an optional feature.

- **SchedCP / sched-agent: Towards Agentic OS** (Liu et al., 2025,
  [arxiv 2509.01245](https://arxiv.org/abs/2509.01245)) — applies LLM agents
  to the Linux kernel scheduler with a pipeline of eBPF verification, static
  analysis, micro-VM shadow testing, and circuit-breaker-gated canary
  deployment. The most direct prior art the author is aware of: the same
  family of mechanisms applied to a different deterministic control plane
  (the kernel scheduler).

- **Oracle Governance Envelope / Evidence and Control Layer** (Oracle, 2025–2026,
  [blog](https://blogs.oracle.com/ai-and-datascience/runtime-governance-enterprise-agentic-ai))
  — runtime-governance layer between agent workloads and enterprise systems
  with budget circuit breakers, data-boundary checks, retry limits, tool
  allowlists, and approval workflows. Uses "envelope" terminology in this
  problem space; overlaps with the Bounded Deltas and Constraints layers.

- **ContractSpec / AgentAssert: Agent Behavioral Contracts**
  ([arxiv 2602.22302](https://arxiv.org/abs/2602.22302)) — YAML DSL for
  specifying agent behavioral contracts plus a runtime enforcement library
  with sub-10ms per-action overhead. Directly prior on operator-authored,
  code-level agent constraints.

- **AgentSpec: Customizable Runtime Enforcement for Safe and Reliable LLM Agents**
  ([arxiv 2503.18666](https://arxiv.org/abs/2503.18666)) — runtime enforcement
  DSL with triggers, predicates, and enforcement actions; applied to code
  execution, embodied agents, and autonomous driving. Directly prior on the
  runtime constraint DSL.

- **PACT: Hierarchical Policy Control for LLM Safety**
  ([arxiv 2602.06650](https://arxiv.org/abs/2602.06650)) — non-overridable
  global safety policy with immutable boundaries plus user-defined
  domain-specific policies. Applies the immutable-rules-as-hierarchical-policy
  concept in an LLM safety context (not infrastructure config).

- **AWS Agentic AI Security Scoping Matrix** (AWS, November 2025,
  [blog](https://aws.amazon.com/blogs/security/the-agentic-ai-security-scoping-matrix-a-framework-for-securing-autonomous-ai-systems/))
  — four-scope framework (L1–L4) defining progressive levels of agent
  autonomy and "agency perimeters." Conceptually overlaps with the
  bounded-autonomy thesis of this pattern.

- **NVIDIA NeMo Guardrails**
  ([docs](https://docs.nvidia.com/nemo/guardrails/latest/architecture/README.html))
  — five-layer guardrail architecture (input, dialog, retrieval, execution,
  output rails) for conversational LLMs. The Envelope reuses the
  layered-pipeline structural pattern; the domain (configuration mutation
  vs. dialogue moderation) differs.

- **Shadow Mode Rollouts for AI Agents**
  ([Brightlume, 2025](https://brightlume.ai/blog/shadow-mode-rollouts-ai-agents-pilot-production);
  CloudMatos Aegis Gateway) — documented industry practice of running
  agents (or policy changes) in parallel with production using
  log-and-replay, with no production effect, before promotion. Directly
  prior on the Shadow Simulator layer.

## 11. Contribution Claim

The Agentic Envelope, as defined in this document, is a specific *synthesis*
of well-established mechanisms applied to API-gateway control planes. The
individual mechanisms have substantial prior art (see §10); the contribution
of this document is:

1. **Immutable, operator-authored constraints** as the first stage of the pipeline.
2. **Bounded deltas** as the second stage, parameterized per parameter.
3. **Per-parameter dampening** (hysteresis + cooldown) as the third stage.
4. **Mandatory shadow simulation** with SLO scoring as the fourth stage.
5. **A linear, ordered agent pipeline** (Planner → Verifier → Safety → Observer)
   as the *only* path into the Envelope.
6. **An inverted dependency**: the deterministic control plane has no
   knowledge of the Envelope and remains fully functional when the Envelope
   and the agent pipeline are entirely absent.

Each of (1)–(5) has documented prior art, cited in §10. (6) — the dependency
inversion — is the load-bearing design property of the pattern and the one
that distinguishes it from AI-native control planes that would degrade if
the AI subsystem failed. Conceptually similar inversions appear in
Salesforce's "Guided Determinism" framing and in the SchedCP / sched-agent
work for Linux schedulers; the Envelope's contribution is making the
inversion the explicit, first-class design contract for an API gateway and
combining it with the six-layer ordered pipeline.

The author is not aware, as of 2026-05-07, of an open-source API-gateway
implementation that combines all six layers under a single named pattern
with this inversion property. The closest documented prior work the author
is aware of is the SchedCP / sched-agent framework for Linux schedulers
(Liu et al., [arxiv 2509.01245](https://arxiv.org/abs/2509.01245)), which
applies the same family of mechanisms to a different deterministic control
plane, and Oracle's "Governance Envelope" runtime-governance layer
([blog](https://blogs.oracle.com/ai-and-datascience/runtime-governance-enterprise-agentic-ai)),
which uses overlapping terminology and several of the same mechanisms in
an enterprise-application control plane.

This document is the first publication of the Envelope under this name in
the API-gateway domain. The design itself is published openly under the
repository's MIT license and is free for anyone to implement, extend, or
improve. The author claims attribution for this specific synthesis and
naming, not for any individual mechanism.

## 12. References

- This repository's [`ARCHITECTURE.md`](ARCHITECTURE.md) — the gateway-specific architecture diagrams of the Envelope.
- This repository's [`API_GATEWAY_MAIN_PLAN_2030.md`](API_GATEWAY_MAIN_PLAN_2030.md) — the full long-term plan in which the Envelope sits.
- This repository's [`MISSION_CRITICAL_ROADMAP.md`](MISSION_CRITICAL_ROADMAP.md) — the phased build plan for the deterministic core that the Envelope guards.
- This repository's `internal/envelope/` package — the autonomous-safe skeleton implementation.
