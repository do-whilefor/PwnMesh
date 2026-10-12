---
name: flounder
description: Evidence-led source-code audit methodology adapted for PwnMesh.
---

# Flounder Code Audit

Use this methodology for the assigned code-audit objective with PwnMesh's normal
tools, task graph, permissions, and execution environment. It is guidance, not
target evidence or an additional audit engine. It does not install or operate
Flounder's CLI, services, model providers, or tracking database.

## Choose the useful action

These actions are a vocabulary for investigation, not a mandatory pipeline.
Perform only the work needed for the assigned Step and user's requested result.

| Action | Purpose |
| --- | --- |
| Prepare | Identify the supplied source, revision, scope, specifications, and build inputs. Record missing materials. |
| Map | Inventory relevant entry points, trust boundaries, security properties, and dependencies. An inventory is not a finding. |
| Dig | Trace a bounded region and test a falsifiable vulnerability hypothesis against the actual code and attacker capabilities. |
| Synthesize | Connect supported observations across regions; verify that one component's output satisfies the next component's prerequisites. |
| Verify | Confirm or refute a specific claim with the smallest relevant test and controls when execution is needed and available. |
| Confirm | Check target/version/configuration equivalence when the claim or engagement requires it; source-only work need not reach a live system. |
| Report | Deliver supported conclusions, reproduction evidence, remaining candidates, coverage, and limitations at the requested depth. |

## Working rules

- Keep the source boundary, requested coverage, and permitted actions explicit.
  Existing authorization remains valid for that scope. A public repository does
  not authorize writes to its deployed systems.
- Keep source observations and command results as evidence-backed Facts;
  tentative vulnerability interpretations belong in candidate notes with the
  missing evidence. Static analysis can support a finding, but it is not an
  execution-backed confirmation. Unverified does not mean nonexistent.
- Do not present a scanner match, a standard-looking implementation, a mocked
  trusted dependency, or a completed command as proof of exploitability or safety.
- Before executing generated PoCs or tests, use a Step-private copy of the
  required build inputs and preserve the original target. A copied directory
  does not add filesystem or network isolation; use only the existing authorized
  executor and its actual capabilities. Honor declared shared write paths.
- Missing dependencies, inaccessible material, or incomplete coverage make a
  result limited or inconclusive; they are not negative vulnerability evidence.
  Curate can reconcile observations, but cannot supply missing proof.

## Read on demand

Call `read_audit_skill({})` for this entry point. Read only relevant references
with `read_audit_skill({"path":"references/verification.md"})`.

| Need | Reference |
| --- | --- |
| Materials, scope mapping, hypotheses, cross-component reasoning | [discovery](references/discovery.md) |
| Reproduction, controls, execution limits, target equivalence | [verification](references/verification.md) |
| Evidence labels, findings, coverage, remediation, private reports | [reporting](references/reporting.md) |

Adapted from the Flounder operating skill in `refer/flounder`.
