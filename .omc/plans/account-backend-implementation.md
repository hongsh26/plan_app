# Account Backend Implementation Plan

## Requirements Summary

- Implement the approved architecture in `docs/account_backend_design.md`.
- Preserve the current runnable iOS app under `src/`.
- Add a Go modular monolith under `server/` with API, worker and scheduler roles.
- Keep PostgreSQL as the source of truth and SwiftData as an iOS projection/cache.
- Enforce authentication, authorization, idempotency, optimistic concurrency and transactional outbox rules at server boundaries.

## Acceptance Criteria

- All criteria in `docs/account_backend_design.md` section 14 are automated where feasible.
- API contract tests prove handler responses conform to OpenAPI.
- PostgreSQL integration tests prove domain state and sync/outbox changes commit atomically.
- The existing iOS project still builds and its existing tests remain green.
- No credentials, tokens or calendar content appear in repository files or logs.

## Implementation Steps

1. Add the `server/` Go module, command entry points, configuration validation and health endpoints described in `docs/account_backend_design.md` section 13.
2. Add SQL migrations for users, identities, devices, sessions, idempotency, sync changes, outbox jobs and audit events, including required partial unique and cursor/range indexes, before Party-domain tables.
3. Implement Apple credential verification/code exchange/provider-token encryption and revocation behind an `IdentityVerifier` interface, plus session rotation/reuse detection.
4. Implement authenticated `/v1/me`, device management and account deletion state transitions.
5. Implement the mutation transaction helper that records idempotency results, sync changes and outbox jobs atomically.
6. Implement cursor sync, bootstrap snapshot/watermark and the iOS SwiftData projection/offline mutation boundary.
7. Implement worker leasing, retry, dead-job handling and APNs adapter boundaries; model EventKit writes as device-executed server commands.
8. Add unit, PostgreSQL integration, API contract and E2E tests from `docs/account_backend_design.md` section 15.
9. Add container and managed-environment deployment assets only after local and CI verification pass.
10. Update `docs/progressing.md` and write a timestamped handoff record after each completed vertical slice.

## Risks and Mitigations

- Apple/APNs external tests need credentials: keep deterministic fakes and run credentialed tests only in an isolated environment.
- Calendar privacy can be violated by broad schemas: enforce allowlisted fields and privacy tests before upload code.
- PostgreSQL job contention may emerge: collect lease wait and retry metrics before introducing another queue.
- Offline conflict UX can diverge from server contracts: share generated API schemas and explicit version-conflict fixtures.
- EventKit is device-authorized: never implement Apple Calendar writes in the server worker; preserve the command/result boundary.
- Current `src/new/*` deletions and untracked `src/*` replacements must be resolved as an intentional rename before any implementation commit.

## Verification Steps

- `go test ./...` including PostgreSQL integration tests.
- OpenAPI validation and handler contract test suite.
- Container health/readiness smoke test.
- iOS targeted unit tests plus project build.
- Secret and sensitive-log scan.
- Independent code review and verifier pass before completion.
