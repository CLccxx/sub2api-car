# Carpool weekly quota floor implementation plan

**Goal:** A valid low estimate after deleting and reconnecting an account must never reduce an automatically calculated per-person weekly carpool cap below $200.

**Design:** Keep the existing calculation order: round the shared estimated total up to the next $50, divide by 2–5 members, round down to cents, then apply a $200 per-person minimum. Replace the old $10 estimate cutoff with a strictly positive estimate check so the reported $5.33 case receives the floor. Zero, negative, missing, stale, nonfinite and excessive estimates retain the existing skip behavior. Keep the 30-minute Shanghai schedule, account eligibility checks, cache invalidation, usage counters, reset windows and raw shared-account estimate unchanged. No schema, infrastructure or UI changes are required. The user has authorized this policy and direct release publication after verification.

**Files:**
- `backend/internal/service/group_carpool.go`: add a named $200 minimum, use `math.Max` after the existing cent calculation, and accept finite positive estimates below $10.
- `backend/internal/service/group_carpool_quota_test.go`: cover the requested low-estimate case, all member counts, floor boundaries, normal estimates and rejected invalid data.
- `deploy/CARPOOL_DEPLOYMENT.md`: describe the minimum and the effective refresh timing.
- `.github/workflows/car-release.yml`: run all service/admin/config unit tests before publication so quota regressions cannot be bypassed by the updater-only name filter.

## Implementation and verification

- [x] Run the existing carpool service tests as the baseline.
- [x] Add and observe failing tests for estimate 5.33 / 3 members, tiny positive estimates, floor boundaries and the real refresh path with 8% upstream usage and window cost `5.33 * 0.08`.
- [x] Update existing low-estimate expectations and move the rounding-only regression above the minimum to keep its independent purpose.
- [x] Implement the minimum and positive-estimate validation; pass carpool/shared-usage tests and formatting checks.
- [x] Obtain independent spec and quality review; pass full backend lint.
- [x] Document the policy without changing raw estimates or resetting usage.

Verification passed: focused carpool/shared-usage tests with an observed red/green
cycle; all unit tests in `internal/service`, `internal/handler/admin` and
`internal/config`; full backend lint with zero issues; independent spec and code
quality reviews. Publication repeats the three unit packages and lint before
building the deployment bundle.

## Release sequence after the source commit

1. Push `main`/`custom`, tag the next unused custom version, and wait for gated release/CI.
2. Verify the public latest bundle, checksum, source commit and installer before directing the user to the web update controls.
