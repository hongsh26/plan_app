# Calendar Privacy and Sync Implementation Plan

## Requirements Summary

- Implement `docs/calendar_privacy_sync_design.md` without weakening `docs/account_backend_design.md`.
- Keep EventKit identifiers and non-allowlisted content on device.
- Separate private busy facts, Party-visible projections and availability results.
- Treat hidden events as Busy for calculation while exposing no event/member cause.
- Execute EventKit write commands only on the designated iOS writer device.

## Acceptance Criteria

- All criteria in design section 14 have automated coverage where feasible.
- Privacy fixtures prove hidden/raw identifiers never appear in API, sync, bootstrap, logs, traces or APNs payloads.
- Snapshot complete is atomic and idempotent.
- Existing app build remains successful and availability tests remain green after implementation.

## Implementation Steps

1. Resolve the current tracked `src/new/*` to untracked `src/*` move before the first implementation commit.
2. Add regression tests around current EventKit reading and hidden exclusion behavior.
3. Split the iOS calendar boundary into authorization, selection, snapshot reading and device command writing.
4. Add SwiftData source mapping, snapshot staging and command result models.
5. Add server calendar migrations, snapshot APIs and atomic generation swap.
6. Add privacy projection and freshness-gated availability services.
7. Add foreground/change-debounced synchronization and best-effort background triggers.
8. Add writer-device command claim/result and EventKit URL marker deduplication.
9. Add permission-revocation, calendar-removal, device-switch and retention cleanup paths.
10. Run iOS, server, PostgreSQL, E2E and privacy validation, then update progress and handoff records.

## Risks and Mitigations

- Aggregate availability can still permit inference: omit member-level causes and rate-limit probing.
- Background delivery is not guaranteed: sync on every foreground activation.
- EventKit identifiers are local and unstable: use device-local opaque mapping and snapshot replace.
- Writer-device loss can leave commands pending: support explicit writer transfer and never blind-create on update.
- Snapshot volume can grow: page uploads and cap the rolling window at 90 days.

## Verification Steps

- Targeted XCTest for EventKit adapter and privacy mapping.
- Go unit and PostgreSQL integration tests for snapshots/projections/freshness/commands.
- OpenAPI contract validation.
- Two-user E2E with all three visibility levels.
- Sensitive-field scan of requests, DB fixtures, logs and APNs payloads.
- Independent reviewer/verifier approval.

