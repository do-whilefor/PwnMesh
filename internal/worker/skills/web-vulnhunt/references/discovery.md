# Asset and Attack-Surface Discovery

Start with the task's concrete inputs. Keep an inventory of asset, source,
observed version, environment, authorization status, and unanswered questions.
An old certificate, source string, or hostname is a lead, not proof of a live
service or permission to test it.

## Local materials

- Inspect supplied HAR files and raw requests with `pwn-http inspect` when
  available. Record methods, paths, required headers, body shape, and account
  context before deciding which request can be replayed. Expired sessions,
  device signatures, and missing bodies can prevent faithful reproduction.
- Read supplied OpenAPI/Postman material and frontend bundles to build a route
  inventory. Record method, parameters, request schema, authentication, and
  expected role or owner. Documentation may be stale; confirm routes selectively.
- Inspect source and configuration for data flows, authentication gates, tenant
  selectors, URL-accepting fields, and public versus private identifiers.
  A keyword, client-side key, or embedded URL does not establish a vulnerability.
- For supplied APK/JAR/DEX files, check their type and contents first. Where
  available, use `jadx` for code and `apktool` for resources and Manifest data.
  Look for API clients, deep links, WebViews, pinning rules, exported components,
  and runtime-derived credentials. Do not execute unknown build or install scripts.

## Live and passive discovery

Use only the sources and request volume appropriate to the assigned scope.
Certificate transparency, DNS, sitemaps, public documentation, response headers,
and known service endpoints can identify candidates. Record their provenance
and avoid automatically probing every discovered name.

- Test DNS assumptions with fresh random labels. A positive result may be a
  wildcard, proxy, captive portal, or resolver synthesis. An empty `dig` result
  can mean NXDOMAIN, timeout, SERVFAIL, or no requested record; inspect status.
- The benchmark range `198.18.0.0/15` includes only `198.18.*` and `198.19.*`.
  An address in it is a reason to investigate resolver behavior, not conclusive
  evidence about whether the target exists. HTTP replies can also be synthetic.
- Certificate entries may identify retired hosts. Validate current ownership,
  scope, DNS, and service behavior before adding a host to the active inventory.
- Customer names, cloud account IDs, and tenant naming patterns help classify
  assets but do not prove ownership or inclusion in an engagement.
- For OAuth/OIDC or SAML discovery, record observed issuers, metadata URLs,
  redirect chains, and relying parties. A discovered identity provider may have
  separate ownership and authorization requirements.

## Prioritization

Build a small map of trust boundaries: anonymous to authenticated, one role to
another, one object owner or tenant to another, browser/mobile client to backend,
and application to an external service. Prioritize routes where the assigned
identities and available evidence can establish an actual boundary failure.
Use [testing](testing.md) for the verification design.
