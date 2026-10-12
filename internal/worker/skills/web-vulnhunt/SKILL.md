---
name: web-vulnhunt
description: Evidence-led methodology for authorized web, API, SaaS, identity, and mobile-backend penetration testing in PwnMesh.
---

# Web Vulnerability Hunting

Use this methodology for the assigned pentest objective. It guides investigation;
it is not target evidence, a scanner, or an instruction to run every technique.
Use the normal task tools to inspect materials, test hypotheses, and save evidence.
The existing task constraints, permissions, and PwnMesh finding rules still apply.

## Scope

Record the authorized assets, identities, permitted actions, and exclusions from
the task once. Honor authorization already given for the same scope. Resolve
missing scope before active testing or expanding to another asset or action.
Discovered tenants, redirects, callback services, and third-party integrations
are candidates for scope review; discovery alone does not authorize testing them.
Use assigned test accounts and data; keep state changes within the agreed scope.

## Investigation

1. Map the supplied URLs, APIs, accounts, roles, tenants, versions, and local
   materials. Separate observed assets from inferred or historical ones.
2. For each candidate, state a falsifiable hypothesis, the expected authorized
   behavior, a control, the smallest useful test, and the evidence needed.
3. Compare equivalent requests while changing one relevant variable, such as
   account, object owner, tenant, token scope, or path. Preserve both sides.
4. Treat status changes, malformed-body errors, SPA fallback, fingerprints,
   exposed interfaces, and static code paths as clues. Verify the actual boundary
   failure and impact before calling a vulnerability confirmed.
5. Record positive, negative, and inconclusive results with their conditions.
   A failed attempt narrows that hypothesis; it does not prove general safety.
6. After a patch, reproduce the original conditions and check relevant variants
   on explicitly covered assets. Separate a closed chain from a remaining primitive.
7. Report reproducible findings, limitations, controls, and remediation. Use the
   built-in `cvss31` tool only after evidence supports all eight Base metrics.
   Otherwise report `Not Scored` and identify the missing evidence.

## Read on demand

Call `read_pentest_skill({"path":"references/testing.md"})` to load a reference.
Calling `read_pentest_skill({})` returns this entry point.

| Investigation need | Reference |
| --- | --- |
| Assets, API inventories, DNS, local HAR/APK/source materials | [discovery](references/discovery.md) |
| Controls, authorization, routing, callbacks, OAuth/SSO/FIDO/mobile | [testing](references/testing.md) |
| Reproduction, patch checks, negative results, vulnerability chains | [verification](references/verification.md) |
| Evidence, finding structure, CVSS, remediation | [reporting](references/reporting.md) |

Adapted from Web-Vulnhunt.
