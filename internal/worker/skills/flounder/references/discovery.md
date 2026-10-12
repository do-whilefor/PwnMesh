# Materials, Mapping, and Investigation

## Prepare only needed materials

Identify the supplied repository or package, revision, source roots, build root,
dependencies, specifications, and task exclusions. Distinguish supplied facts
from inferred deployment details. Use existing materials before fetching more;
acquisition remains bounded by the assigned work and permissions.

Project-owned specifications, design documents, prior audits, and factual incident
material can provide context. They do not prove a current defect. For an explicit
blind audit or recall evaluation, exclude answer-bearing reports and suspected
bug locations unless the evaluation supplies them. For incident investigation,
keep the factual clue separate from a proposed explanation.

## Map relevant security properties

Inventory public and internal entry points, assets, identities, privilege changes,
data flows, trust boundaries, and dependencies relevant to the task. State the
property each region should preserve, such as ownership, authorization, parser
bounds, accounting conservation, signature binding, or proof soundness.

Prioritize regions using actual reachability and impact, uncertain assumptions,
and interactions across boundaries. Keep required but unexamined regions visible.
Do not manufacture findings to fill an inventory or split work merely to occupy
parallel capacity. Plan only through the tools available to the current role.

## Dig and synthesize

1. State a falsifiable hypothesis, affected revision, violated property, attacker
   inputs and privileges, expected behavior, and smallest distinguishing check.
2. Trace the complete relevant path: validation, callers, downstream consumers,
   configuration, and trust assumptions. A suspicious line alone may be guarded
   elsewhere; familiar or upstream code may still be wrong in this context.
3. Preserve source locations and raw observations. Identify what the evidence
   establishes and what remains an interpretation or missing prerequisite.
4. For a proposed chain, identify what each component produces and what the next
   requires. Check identity, encoding, timing, state, and privilege compatibility.
   Separate demonstrated primitives from an unproven end-to-end effect.
5. Revisit coverage when new evidence justifies it. A changed source revision or
   material dependency invalidates conclusions that depended on the old inputs;
   it does not automatically erase unrelated evidence.

Useful contract checks include authorization and reentrancy paths, accounting
invariants, signature replay domains, and oracle assumptions. For ZK/proof code,
check that constraints bind the intended statement and public inputs. These are
hypothesis sources, not mandatory checks or presumed defects.

Use [verification](verification.md) when a claim needs reproduction. Record
coverage and blockers as described in [reporting](reporting.md).
