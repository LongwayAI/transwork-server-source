# Upstream Sync Policy

## Goal

Keep `transwork-server` close enough to `QuantumNous/new-api` that new models, channels, and provider fixes can be adopted quickly, while keeping Gressio-specific behavior in a thin overlay that is cheap to re-apply.

This policy is written for the repo as it exists today, not just for the future ideal state.

## Strategic Direction

The default strategy is a tracking fork, not a hard fork:

- Upstream remains the primary source for generic relay, model, and channel behavior.
- Gressio carries only product-specific logic.
- We sync from upstream regularly enough that new model support does not require emergency fork patches.

Revisit that choice only if Gressio-specific product code starts to dominate upstream-owned relay code. If most future work looks like more ElevenLabs- or GCS-style product flows, and upstream syncs become mostly conflict resolution, then the strategy should be reconsidered as a hard fork with selective upstream cherry-picks.

## Current Reality

The current history is not yet shaped like a clean overlay:

- `3524ee8a` is the fork bootstrap commit.
- `d05723c3`, `c3df8d82`, `0120dbca`, `13b5f1dd`, and `dd0d80a6` are real Gressio changes.
- `9135037c` came through a merged side branch and touches core relay behavior.
- `2fd377cc` is a bundled blob that mixes GCS, batch ASR, vendored dependencies, `.env`, and large docs.

That means the first migration onto fresh upstream is not a clean cherry-pick exercise. It is a controlled manual port with selective cherry-picks where they are actually clean.

## Branch Model

Use names that match the repo people are already working in:

- `main`
  - Primary integration branch on `origin`.
  - Should always represent "Gressio on a recent upstream base".
- `sync/YYYYMMDD-upstream`
  - Short-lived upstream integration branch cut from `upstream/main`.
- `work/*`
  - Short-lived feature branches.
- `release/*`
  - Optional release branches for deployed snapshots.

Do not document a long-lived `transwork/main` branch unless the repo is actually renamed to use it. Today, contributors should assume `main` is the integration branch.

## Remote Setup

- `upstream` -> `https://github.com/QuantumNous/new-api.git`
- `origin` -> `https://github.com/LongwayAI/transwork-server.git`

Example:

```bash
git remote add upstream https://github.com/QuantumNous/new-api.git
git fetch upstream
```

## Sync Trigger

Cadence without a trigger slips. Use both:

- A scheduled reminder every 2-4 weeks.
- An immediate sync whenever upstream lands a model, provider, or channel change that Gressio needs.

Preferred implementation:

- A scheduled CI job or automation reminder creates a `sync/YYYYMMDD-upstream` branch or PR from `upstream/main`.

If automation is not added yet, assign a calendar owner and keep the cadence explicit in team operations.

## First Migration Rule

Before adopting the recurring sync workflow, accept that the first rebase onto fresh upstream is a cleanup migration.

Required pre-step:

1. Treat `2fd377cc` as a manual port, not a cherry-pick.
2. Split its intent into logical buckets:
   - GCS storage support
   - batch ASR flow
   - dependency changes
   - docs
3. Do not carry `.env`, local binaries, logs, or database state into the new overlay history.
4. Do not blindly replay vendored dependency trees from the old fork.

After that first migration, future overlay commits must be kept small enough to replay cleanly.

## Sync Workflow

1. Fetch upstream.
2. Create a fresh `sync/YYYYMMDD-upstream` branch from `upstream/main`.
3. Re-apply the Gressio overlay on top.
4. Run tests and targeted manual verification.
5. Fast-forward `main` only after verification passes.

Recommended flow:

```bash
git fetch upstream
git checkout -b sync/YYYYMMDD-upstream upstream/main
```

Then re-apply Gressio changes by:

- cherry-picking clean overlay commits
- manually porting high-conflict features
- rebuilding a small Gressio-only subtree of files when the boundary is clear

Avoid:

- `git pull upstream main` on a long-lived dirty branch
- giant merge commits after large drift
- carrying local logs, databases, or binaries in integration branches

### Disposable Sync Worktree

The sync branch should usually be checked out in a disposable worktree under `.sync/YYYYMMDD-upstream/`.

Policy:

- Treat `.sync/` as local workspace, not repository content.
- Do not commit `.sync/*`.
- Keep only one active sync worktree unless parallel sync work is intentional.
- Remove the worktree with `git worktree remove .sync/YYYYMMDD-upstream` after the sync lands or is abandoned.

## Vendor Policy

Dependency vendoring must be explicit because `vendor/` is a major conflict source.

Policy:

- Default to Go modules as the source of truth: `go.mod` and `go.sum`.
- Do not treat `vendor/` as authoritative overlay content.
- Do not port old vendored trees forward during upstream syncs.
- Only keep `vendor/` committed if there is a real deployment or compliance requirement that cannot rely on module download.

If vendoring is required:

- Regenerate it from the new integration branch after dependency reconciliation.
- Keep vendoring changes in their own commit.
- Never bundle product logic with vendored dependency updates.

If vendoring is not required:

- Remove `vendor/` from the maintained overlay and rely on `go mod tidy` plus the module proxy.

Current decision for this migration:

- Do not port the old fork's vendored dependency tree into the upstream sync branch.
- Treat `go.mod` and `go.sum` as the authoritative dependency state for the rebuilt overlay.

## Ownership Rules

### Upstream-Owned First

These paths should stay as close to upstream as possible:

- `relay/`
- `router/relay-router.go`
- `controller/channel.go`
- `service/convert.go`
- `common/endpoint_type.go`
- `common/model.go`

If a Gressio feature needs these files, prefer a thin hook, feature flag, or helper package over a broad rewrite.

### Gressio Overlay

These are reasonable places for Gressio-owned behavior:

- `transwork/handler/elevenlabs.go`
- `transwork/handler/audio_transcribe.go`
- `transwork/storage/gcs.go`
- `transwork/routes.go`
- `transwork/init.go`
- `transwork/scripts/rebuild-local.ps1`
- `transwork/scripts/rebuild-local.sh`
- `transwork/scripts/start-local.ps1`
- `transwork/scripts/start-local.sh`
- `transwork/scripts/stop-local.ps1`
- `transwork/scripts/stop-local.sh`
- Gressio-only docs under `transwork/docs/`

### Target Boundary To Move Toward

Over time, move Gressio-specific logic behind dedicated registration and helper seams:

- `transwork/routes.go`
  - Register Gressio-only admin or product routes.
- `transwork/handler/*`
  - Product-specific handlers such as ElevenLabs token helpers and batch ASR flows.
- `transwork/storage/*`
  - GCS upload/download helpers and provider-specific orchestration.
- `relay/channel/transwork/*`
  - Only if a provider adaptor is truly custom and not upstream-worthy.

The goal is to stop editing upstream core files unless there is no clean seam.

## Current Hotspots

These are the current high-conflict areas for future upstream syncs:

- `router/api-router.go`
  - Should only carry thin Gressio registration hooks such as Codex OAuth.
- `relay/channel/elevenlabs/*`
  - Custom realtime and file transcription behavior.
- `transwork/handler/audio_transcribe.go`
  - Large custom batch ASR flow with GCS and ElevenLabs coupling.
- `transwork/storage/gcs.go`
  - Product-specific storage integration.
- `service/convert.go`
  - Cross-provider conversion logic with Gressio-specific changes.
- `relay/channel/openai/adaptor.go`
- `relay/channel/openrouter/adaptor.go`
- `relay/relay_adaptor.go`
- `controller/channel.go`
  - Any local edits here will conflict frequently with upstream model and channel churn.

## Conflict Resolution Authority

When sync conflicts happen in upstream-owned files, use this rule:

- Default to upstream behavior unless the conflict is required for a documented Gressio product feature.
- The maintainer performing the sync proposes the resolution.
- If the conflict affects routing, auth, billing, or provider correctness, require explicit review from the repo owner before merge.

This avoids silent drift while keeping product-specific exceptions intentional.

## Commit Policy

Future Gressio overlay commits must stay small and replayable:

- one commit for local helper scripts
- one commit for ElevenLabs realtime routing
- one commit for ElevenLabs temp token API
- one commit for GCS support
- one commit for batch ASR behavior
- one commit for vendoring, only if vendoring is intentionally retained

Do not bundle:

- local environment changes
- generated binaries
- logs
- database files
- vendored dependencies with feature logic
- unrelated docs with runtime code

## Files That Should Not Live In Integration Branches

These should be ignored locally, not treated as real overlay content:

- `.env`
- `one-api.db`
- `logs/*`
- `transwork-server.local.exe`
- `transwork-server.local.out.log`
- `transwork-server.local.err.log`

## Decision Rule For New Customizations

Before changing an upstream core file, ask:

1. Can this live in a new controller, middleware, or helper package instead?
2. Can this be registered from a Gressio-only router file?
3. Is this generic enough to upstream back to `new-api`?

If the answer to any of those is yes, avoid modifying the upstream core path directly.

## Recommended Immediate Next Steps

1. Add an `upstream` remote permanently.
2. Start a fresh `sync/YYYYMMDD-upstream` branch from current `upstream/main`.
3. Treat `2fd377cc` as a manual port and split its concerns while rebuilding the overlay.
4. Decide whether `vendor/` stays in policy or is removed from maintained history.
5. Move custom route registration into a Gressio-specific router registration file.
6. Add or tighten ignore rules for local artifacts.

## Landing Checklist

Before promoting a sync branch back to `main`:

1. Split the overlay into small replayable commits.
2. Run targeted Go tests for the touched packages.
3. Run manual verification for Gressio-only flows with live credentials.
4. Confirm `.env`, logs, local binaries, database state, and `.sync/` are not being carried into maintained history.
5. Remove the disposable sync worktree after the sync is merged or otherwise landed.
