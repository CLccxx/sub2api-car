# Usage presentation and official 0.2.20 integration

Date: 2026-11-10. Starting custom release: v0.2.19-car.3.

## Design and scope

Preserve the product's existing restrained light/dark UI. Per the user's explicit
design correction, account-list costs must use the same compact chips as the
existing `U $1940` statistic, prefixed with short localized labels `订阅` and
`点数`. Full meanings and frozen/legacy information remain available in accessible
text and tooltips. In My Subscriptions, keep every label near its value: the
reset countdown must sit beside or directly below its caption rather than at
the far right. Group seven-day usage/reset, the two costs, then remaining credits
and reset actions. Use spacing and separators rather than nested cards.
Keep all existing API contracts, currency values, estimate rules, refresh cooldown,
reset voting, expired/unavailable states, and other account platforms intact.

## Execution

- [x] Refine UsageProgressBar.vue, SharedAccountUsage.vue and the shared reset
  panel's presentation. Run their existing component regressions and frontend
  typecheck/lint/build. Inspect desktop/narrow, both themes and languages, frozen,
  dynamic, legacy and unavailable states in the actual rendered components.
- [ ] Commit verified UI changes before merging the official release tag.
- [ ] Fetch and verify official v0.2.20; review its changes and merge the exact
  tag on this isolated branch. Resolve conflicts preserving both official fixes
  and custom subscription/carpool/updater functionality. Check migration filename
  and ledger compatibility, gateway accounting hooks, frontend APIs and CI tooling.
- [ ] Run backend unit/integration/PostgreSQL upgrade checks, frontend regression
  suite, static checks and embedded build. Independently review specification
  compliance and code quality; fix actionable findings.
- [ ] Push PR, wait for CI, merge into remote main, create the next available
  v0.2.20-car.N tag, follow release workflow, then verify formal Latest status,
  complete published assets, checksums, migration catalog and updater discovery.

UI ownership: assigned UI implementation agent. Parent owns integration,
documentation, visual checks and publication. No production-server deployment is
requested; publication enables the existing web updater.

UI verification: 166 focused component/locale tests, typecheck, full lint and
production build passed. Rendered desktop and 390px views were inspected in
Chinese/English and light/dark themes, including dynamic/frozen/legacy/unknown
states. Independent specification and quality reviews passed.
