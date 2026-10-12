# Reproduction and Target Equivalence

Verification answers a specific claim. Decide what evidence the requested result
needs before running commands; a static-review request does not automatically
authorize deployment testing or require a new execution environment.

## Prepare a bounded reproduction

- Pin the source revision and relevant dependency, toolchain, configuration, and
  initial-state assumptions. Record gaps that could change the outcome.
- Before executing generated PoCs or tests, copy the needed build inputs into a
  Step-private workspace. Keep generated tests and outputs there; do not patch
  the original target merely to make a reproduction pass. Explicitly assigned
  remediation remains separate work and should preserve the original evidence.
- Use the existing authorized executor. Copying files does not create a sandbox
  or isolate network, secrets, mounts, or host resources. Do not infer additional
  isolation, privileges, network access, or tool availability from this guide.
- Keep dependency installation and network use within the task's existing
  permissions. If execution cannot be supported, retain source evidence and
  name the blocker instead of silently changing deployment or model settings.

## Execute the smallest useful check

1. Establish a control that exercises the relevant path under valid conditions.
2. Change the attacker-controlled input or state needed for the hypothesis.
   Privileged setup is not an attacker capability unless the claim grants it.
3. Capture the exact command, relevant test/PoC files, exit status, output, and
   observable security-property failure. A crash or assertion must demonstrate
   the claimed effect, not just malformed input or a broken test environment.
4. Check that mocks preserve the behavior relevant to the claim. Replacing a
   trusted component with attacker behavior cannot prove that behavior is
   reachable in the real component.
5. If a minimal-fix comparison is useful and authorized, apply it in a separate
   copy and repeat the same test and controls. Distinguish a fix from disabled
   functionality, changed credentials, configuration drift, or a skipped path.

Retain positive, negative, and inconclusive outcomes. A failed reproduction may
weaken the hypothesis, expose an invalid assumption, or indicate an incomplete
environment. Explain which; do not convert infrastructure failure into refutation.
Stop repetitions when the evidence resolves the assigned question or the task's
limits are reached.

## Confirm only the required equivalence

For a source-only audit, local evidence may satisfy the requested result. If a
claim refers to a deployed target, establish the relevant source/version,
configuration, enabled feature, and attacker-reachability correspondence. State
whether evidence comes from a unit test, local integration, local fork, or an
authorized deployed-system observation.

Do not require live-system exploitation for every finding or treat a local fork
as proof that a production attack occurred. Prefer local replay for transactions
and state-changing exploits; do not broadcast, move funds, persist access, or
write to deployed systems without scope explicitly covering that action. If
equivalence is unknown, report the supported local result and the gap.

Use [reporting](reporting.md) to keep evidence strength and limitations explicit.
