# Findings, Evidence, and Coverage

Report the requested result, not a transcript of every action. Use PwnMesh's
existing Facts, candidate notes, Step results, and evidence artifacts. The labels
below describe evidence in prose; they do not create graph fields or a separate
tracking lifecycle.

## Keep evidence strength explicit

| Description | Required support |
| --- | --- |
| Source-supported candidate or static finding | Cited code, the violated property, an explained path and preconditions, plus missing execution or reachability evidence. |
| Locally execution-confirmed | An actual command/test demonstrated the claimed failure with relevant controls and realistic attacker capabilities. |
| Differentially confirmed | The same reproduction demonstrated the failure before a minimal change and was blocked afterward, with valid controls. |
| Target-equivalent reproduction | Relevant source, version, configuration, and environment correspondence is supported; identify the actual execution boundary. |
| Refuted under tested conditions | Evidence contradicts a specific claim under stated conditions, rather than merely failing to run it. |
| Inconclusive | Missing materials, environment failures, or unresolved assumptions prevent the claimed conclusion. |

Static findings can be useful and should not disappear because execution is
unavailable. Label their evidence accurately and separate them from reproduced
vulnerabilities. Facts may describe observed code and test outcomes; unsupported
vulnerability interpretations remain candidates. Curate cannot promote a claim
through agreement alone.

## A useful finding

Include the affected revision and location, violated security property, attacker
prerequisites, supported impact, evidence level, and reproduction conditions.
Reference raw command evidence and PoC/test paths when available. Explain controls,
counterevidence, target-equivalence gaps, and a focused remediation when requested.
Preserve confirmed primitives separately from unproven chains. Deduplicate by
root cause and demonstrated effect while retaining distinct affected conditions.

Do not equate a tracking decision, scanner severity, reward estimate, or duplicate
uncertainty with technical proof. Follow an explicitly requested scoring method
only with supporting evidence; do not invent a numerical score.

## Completion and limitations

State the inspected source and revision, covered properties or regions, unresolved
candidates, required but unexamined scope, and material or execution blockers.
Separate missing resources and coverage work from findings. Zero reproduced
findings in a limited run means "no confirmed finding in the covered scope";
it does not establish that the target is safe.

Deliver reports at the requested depth; an assigned local review does not require
a bounty submission package. If disclosure is requested, check the specified
venue and terms separately from evidence strength. Do not publish or submit
without explicit authorization. Exclude credentials, private data, and irrelevant
local paths; use repository-relative source locations and scoped evidence.
