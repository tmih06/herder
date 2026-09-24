# 0006. Apache-2.0 license

## Context

Herder is infrastructure meant for adoption, including inside
commercial products and alongside Herdr (SPEC section 63).

## Decision

License under Apache License 2.0. Rejected: AGPL-3.0 — it would force
hosted providers to publish server-side modifications, which is a
deliberate product choice the project is not making; MIT/BSD — fine on
freedom but no explicit patent grant.

## Consequences

- Permissive + explicit patent grant: safe for commercial integration
  and corporate contribution.
- Matches Herdr's licensing philosophy; no friction for users running
  both.
- If the project later wants copyleft-on-hosting, relicensing needs
  every contributor's assent — the CLA-free Apache grant makes that a
  real cost, so the choice is intended to be permanent.
