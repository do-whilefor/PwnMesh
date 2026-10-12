# Finding and Report Structure

Follow the task's requested output contract. The following fields guide the
content of findings and supporting artifacts; they do not replace graph or
result schemas required by PwnMesh.

## Minimum useful finding

1. **Claim and status:** concise title; verified vulnerability, candidate, or
   observation; security boundary and actual impact.
2. **Affected assets:** tested URL/component, version, role, account/tenant
   context, scope, and conditions. Identify the tested set explicitly.
3. **Prerequisites and reproduction:** ordered steps with exact request shape,
   required test permissions, relevant session state, expected behavior, and
   observed behavior. Use placeholders for secrets in the shared report.
4. **Evidence and controls:** secure paths to complete request/response captures,
   before/after state, callback/device evidence when applicable, and the control
   result. Keep evidence attributable to this run and target.
5. **Impact:** demonstrated confidentiality, integrity, or availability loss;
   distinguish the demonstrated effect from possible downstream consequences.
6. **Score:** `cvss31` result and evidence for every metric, or `Not Scored`
   with the missing information. Add CWE mapping only when supported.
7. **Remediation and verification:** address the failed boundary, specify a
   repeatable acceptance check, and record relevant patch/tenant/version results.
8. **Limitations:** incomplete steps, dependencies, coverage, alternative
   explanations, and remaining uncertainty.

## CVSS 3.1 discipline

Use the built-in `cvss31` tool for Base calculations. For a verified finding
whose evidence supports a vector, the tool accepts this form:

`cvss31({"vector":"AV:N/AC:L/PR:L/UI:N/S:U/C:L/I:L/A:N"})`

The example illustrates syntax only; it is not a default vector. Record the
returned normalized vector, base score, severity, and metric justification.

- **AV/AC:** show how the attack reaches the vulnerable component and what
  conditions beyond the attacker's control are required.
- **PR/UI:** include prior privilege and required human interaction. A leaked
  credential does not automatically erase the privilege needed to obtain it.
- **S:** identify a changed security authorization boundary. Another host,
  domain, tenant, or service name alone does not establish Scope Changed.
- **C/I/A:** use actual demonstrated impact on the affected system. Do not
  infer account takeover from a challenge, callback, token, or partial chain.
- Keep component and chain claims distinct. If a chain is demonstrated, score
  its evidenced outcome and real prerequisites; do not add component scores.

Insufficient evidence means `Not Scored`, not a guessed low score. CVSS is a
severity calculation and does not prove a vulnerability exists.

## Evidence handling and corrections

Keep original evidence in the task workspace under appropriate access controls.
Redact credentials and unnecessary personal data from report excerpts. Do not
upload reports or evidence to external services unless the task authorizes it.

When verification changes a conclusion, update the finding and record what
changed and why. Include relevant negative results and blocked escalation steps.
Treat historical examples, CVE names, and vendor claims as research leads until
matched to the current target and verified with appropriate evidence.
