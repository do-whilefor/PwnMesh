# Hypothesis-First Testing

For each test, record the hypothesis, expected permission boundary, prerequisite
identity, baseline, negative control, request, result, and limiting conditions.
Use bounded requests and assigned test records. Preserve complete evidence in
the task workspace; avoid exposing credentials in progress logs or reports.

## Differential claims

Compare a known test identifier with a fresh control identifier using the same
protocol. A random value is unlikely to exist, not guaranteed absent; prefer a
known absent test fixture when available. Use reserved example domains for
synthetic addresses, and consider whether the endpoint sends mail or creates state.

Hold account, headers, payload shape, and session conditions constant. Repeat
or reverse request order when rate limits, caches, lockouts, and counters could
explain the difference. Compare semantic response fields and body content.
Raw timing equality is not a useful control: timing claims require repeated
measurements and analysis of noise, order, and alternative explanations.

## Authorization and routing

- Create an identity by endpoint matrix: anonymous, assigned low-privilege
  account, privileged control account, object owner, and another assigned tenant
  where covered. Record the expected permission for each pair before testing.
- Use schema-valid inputs. A malformed-body `400` can precede authorization
  checks and does not establish a bypass; framework ordering is target-specific.
- A `200` JSON response may contain an error or permitted public data. Confirm
  unauthorized access to the protected object or an independently observed
  operation. Do not infer a bypass from a status code alone.
- Compare suspicious HTML with a known missing-route or SPA baseline. `200`
  HTML and `405` can have many causes; neither proves static fallback or a
  controller hit. Inspect content, headers, route behavior, and actual effects.
- If path normalization is relevant, test a small ordered set of case, slash,
  encoding, dot-segment, or matrix-parameter variants. Capture the exact sent
  path, accounting for client normalization. Responses suggest routing layers;
  they do not prove which middleware or annotation enforced access.

## URL fetching and mobile backends

Use a task-approved callback receiver and a unique correlation value when an
outbound request is needed to prove a URL-handling issue. Do not send secrets
or target data to a public callback service by default. Correlate the trigger,
receiver capture, timestamps, and application behavior; a User-Agent alone
does not identify the sender conclusively. Do not infer internal access or
data disclosure from a callback alone.

Static mobile analysis identifies paths to investigate. Dynamic proof may need
an authorized device, matching app version, runtime instrumentation, and network
captures. Frida and an authenticator are optional dependencies, not assumed
capabilities. Record missing dependencies instead of claiming runtime validation.

## OAuth, SSO, and FIDO/WebAuthn

- For OAuth/OIDC/SAML, examine issuer, audience, signature validation, redirect
  binding, state/nonce, PKCE where applicable, token scope, and expiry. Use
  assigned identities to test the relevant boundary; metadata is not proof.
- Decode discovered tokens locally first. Distinguish claims from actual
  permissions. Validate credentials only within the task's authorized actions
  and record live reach separately from theoretical chains.
- For WebAuthn, inspect challenge freshness and binding, origin/RP ID checks,
  user/account binding, user verification requirements, and credential ownership
  across registration and authentication. Attestation `fmt: none` is not itself
  a vulnerability. A credential list may be expected protocol behavior.
- For FIDO UAF or device-specific flows, distinguish accepted requests, issued
  challenges, completed credential registration, and successful authentication.
  An echoed session ID or a server error alone does not prove fixation or DoS.
  Any state-changing proof must use the accounts and actions covered by the task.

Use [verification](verification.md) to assess closure and remaining gaps.
