# Reproduction and Post-Patch Verification

Verification closes a specific claim with reproducible evidence. Keep the
target, version, identity, tenant, input, session state, and observed effect
attached to each result. Label observations, interpretations, and unknowns.

## Reproduce before concluding

1. Reproduce the original request and expected security boundary with the
   permitted test data. Preserve the exact request and full response securely.
2. Use an authorized control to distinguish absent routes, invalid credentials,
   malformed inputs, and permission checks from a valid test of the hypothesis.
3. Confirm the protected data or operation independently where possible. For
   changes, inspect before/after state and restore test fixtures if required.
4. Repeat enough to rule out a transient result without exceeding the task's
   request budget or permitted impact. Document instability rather than hiding it.
5. If the chain cannot complete, state the blocked step and dependency. A static
   path, scanner alert, or attempted payload is not dynamic verification.

Placeholder identifiers do not guarantee safety. An endpoint may create state,
apply a default, or trigger side effects even for an unknown ID. Use actions
and fixtures authorized for that test, and observe their effects.

## After a reported patch

- Re-run the original reproduction with a valid, appropriately scoped test
  identity. Renew credentials through the agreed test flow when necessary;
  credential minting is not an automatic prerequisite for patch verification.
- Compare before/after requests, responses, and impact. A `401` caused by an
  expired token does not prove the vulnerability was fixed. A `403` alone does
  not identify a code fix, annotation, or gateway rule.
- Check relevant adjacent routes or normalization variants only when supported
  by the hypothesis and scope. Exhaustive variant sweeps are not mandatory.
- Compare explicitly covered tenants or versions where deployment differences
  matter. Report the tested set; do not generalize to every customer or tenant.
- Distinguish a removed endpoint, changed permissions, disabled feature,
  environment difference, and non-reproducible result. Record uncertainty about
  the underlying patch mechanism when source or stronger evidence is unavailable.

## Primitives and chains

For each proposed chain, write what step A produces, what step B requires, and
whether the value crosses the required account, tenant, scope, time, and trust
boundaries. A token that exists may be unusable; a live credential may still
lack the permission needed for the next step.

Keep confirmed component findings separate from a demonstrated end-to-end chain.
If a patch closes a downstream step while leaving a primitive, record both states.
Score only current, evidenced impact with `cvss31`; do not assign impact merely
because a future defect could reactivate the chain. State missing evidence when
the current primitive cannot be scored as a verified vulnerability.

## Negative and inconclusive results

Record the hypothesis, tested assets, inputs, controls, observed outcome, and
reason the result weakens or fails to resolve the hypothesis. Scope conclusions
to the versions and conditions tested. Revisit a negative result when a relevant
condition changes. Do not carry another engagement's negative results forward
as facts about the current target.

Use [reporting](reporting.md) to deliver the evidence and limitations.
