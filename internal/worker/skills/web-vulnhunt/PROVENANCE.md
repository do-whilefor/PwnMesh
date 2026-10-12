# Provenance and Adaptation

This methodology is adapted from the repository's `refer/Web-Vulnhunt` package,
copyright 2026 w1th0ut (U-Sec / 无界安全), distributed under the [MIT license](LICENSE).
The retained license is copied without modification. The upstream package is
reference material; its engagement claims have not been independently verified.

The PwnMesh adaptation retains hypothesis-driven discovery, controls against
false positives, web/API authorization analysis, identity and mobile workflow
checks, post-patch verification, evidence-based reporting, and CVSS discipline.
Read [SKILL.md](SKILL.md) for the entry point and available references.

## Runtime use

The methodology is served by the read-only `read_pentest_skill` tool in eligible
pentest execution phases. References are loaded on demand. It does not install
tools, contact targets, execute shell templates, or establish target evidence.
Normal task tools perform the assigned work. Availability of optional utilities,
devices, authenticators, and callback receivers must be checked when needed.

## Intentional omissions and corrections

- The six upstream scripts are omitted. PwnMesh already provides `cvss31`,
  and the shell templates contain engagement-specific targets and side effects
  that should not be scheduled automatically from a methodology document.
- The upstream calculator and scoring reference use an incorrect Scope Changed
  impact equation. They are not imported. PwnMesh's existing CVSS 3.1 calculator
  remains the calculation authority; the adaptation supplies evidence guidance.
- The upstream negative-control harness compares `time_total` for exact equality
  alongside response data. Normal timing variation can therefore create a false
  differential. The adaptation requires semantic controls and repeated timing
  measurements when a timing claim is relevant.
- Several upstream scripts delete their temporary evidence directory at exit
  while telling the operator to inspect that directory. The adaptation requires
  retained task evidence. It also avoids fixed shared response paths and printing
  tokens or token prefixes in script output.
- The DNS template incorrectly matches `198.18` through `198.31` as the benchmark
  `/15` range and treats an empty lookup as NXDOMAIN. The discovery reference
  corrects the range and distinguishes resolver failures, synthesis, and wildcards.
- Historical AcmeAuth outcomes, counts, CVE claims, tenant names, token lifetimes,
  framework ordering, and findings are not promoted into universal facts. Status
  codes, credential issuance, `fmt: none`, and framework fingerprints need current
  target context and impact evidence.
- Source instructions about `~/.zcode`, the TianTi repository's Git rules,
  blanket customer-tenant exclusions, and conflicting credential-validation rules
  are not imported. Actual task authorization, exclusions, and PwnMesh constraints
  govern the work; existing authorization does not need repeated confirmation.
- Public callback services, all-tenant sweeps, automatic credential minting, and
  destructive placeholder requests are not prescribed. Callbacks and tests must
  fit the task's scope; placeholder identifiers alone do not guarantee no effects.

The source's review log and engagement-specific killed-hypothesis/CVE lists are
not bundled. Their useful process lessons appear in the focused references,
without presenting historical assertions as evidence about the current target.
