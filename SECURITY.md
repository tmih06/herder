# Security

## Supported versions

Only the latest `v0.x` release receives security fixes. There is no
backport line yet; upgrade to the newest tag.

| Version      | Supported |
| ------------ | --------- |
| latest v0.x  | yes       |
| older tags   | no        |

## Reporting a vulnerability

Report privately through GitHub: **Security → Advisories → Report a
vulnerability** on this repository (private vulnerability reporting).
Do not open a public issue for a suspected vulnerability.

Include: affected version/commit, the component (`internal/...`), a
reproduction or the invariant you believe is broken, and impact. You get
an acknowledgement; fixes ship in the next release with credit unless
you opt out.

For non-security bugs, use the normal issue tracker.

## Scope

Herder's security model is a trust hierarchy: the controller is the most
trusted component and the coding agent is treated as potentially
compromised, so authority is enforced structurally — credential
boundaries, least-privilege sandboxes, and a no-raw-Herdr-socket
invariant — never through prompts. The enforced-vs-aspirational split
for v0.1 is documented in
[docs/security-boundary.md](docs/security-boundary.md); the threat model
and rationale live in SPEC.md sections 28-32 and 65-66.
