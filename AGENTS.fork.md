# Fork Maintenance Guide

This file exists only in `battika/slackdump`, a fork of `rusq/slackdump`.
Upstream has no file by this name, which is deliberate: a file upstream does
not have can never produce a merge conflict. Prefer adding to this file over
editing `AGENTS.md`, which upstream owns and revises.

Read this before syncing with upstream or cutting a release. Read `AGENTS.md`
for the project's own conventions — they still apply here in full.

## What this fork adds

Everything tracks upstream except the built-in viewer, which gains:

| Feature | Where it lives |
|---|---|
| Paged channel timelines (`-page-size`) | `internal/viewer/paging.go`, `handlers.go` |
| Archive search, global and per-conversation | `internal/viewer/search.go`, `templates/index.html` |
| FTS5 word search + `LIKE` substring search | `internal/chunk/backend/dbase/repository/fts.go`, `ftsquery.go`, `dbmessage.go` |
| Collapsible sidebar groups | `internal/viewer/templates/index.html` |

The binding invariants for each live in `internal/viewer/AGENTS.md` and
`internal/chunk/backend/dbase/AGENTS.md`. **Read those before changing viewer
or database code** — they record decisions that are expensive to rediscover,
including several that were rediscovered the hard way.

## One-time setup

The fork is normally cloned with only `origin`. Add upstream before the first
sync:

```bash
git remote add upstream https://github.com/rusq/slackdump.git
git fetch upstream --tags
```

Fetching tags is local-only and safe. Pushing them is not — see below.

## When a new upstream release lands

### 1. Merge, do not rebase

```bash
git fetch upstream
git merge upstream/master
```

`origin/master` is public and this fork does not force-push, so rebasing is
wrong here: it would rewrite commits already published. A merge commit is the
correct outcome even though feature work itself is squashed into single
commits.

### 2. Expect conflicts in exactly these places

Files this fork has modified, in rough order of how likely upstream is to touch
them:

- `cmd/slackdump/internal/man/assets/changelog.md` — near-certain. Upstream
  moves its `## Unreleased` entries under a version heading; this fork's three
  entries (paging, search, search modes) sit in the same section. Keep both,
  under whatever heading upstream settled on.
- `README.md` — the fork banner near the top and the `# Fork Additions`
  section.
- `internal/viewer/**` — the feature surface. Templates and handlers are the
  most likely to move under you.
- `.goreleaser.yaml` — this fork narrows the build matrix and drops `nfpms`.
- `internal/chunk/backend/dbase/**` — the FTS work.

`.github/workflows/**` should **not** conflict, and that is intentional: the
Docker workflows are disabled server-side rather than deleted (see below).

### 3. Verify, in this order

```bash
go build ./... && go vet ./... && gofmt -l internal/ cmd/
make test
```

Then the two checks that generic verification will not catch:

```bash
# The interop guard. If this fails the fork is unshippable, however green
# everything else is -- see invariant 22 in the dbase AGENTS.md.
go test ./internal/chunk/backend/dbase/repository/ -run TestEnsureFTS -v
```

```bash
# Does an archive this fork has indexed still open in the NEW upstream build?
git worktree add /tmp/sd-upstream vX.Y.Z
(cd /tmp/sd-upstream && go build -o /tmp/sd-upstream-bin ./cmd/slackdump)
go build -o /tmp/sd-fork ./cmd/slackdump

cp -R <archive-dir> /tmp/sd-interop            # a COPY, always
/tmp/sd-fork view -listen 127.0.0.1:38500 /tmp/sd-interop &
curl -s 'http://127.0.0.1:38500/search?q=meeting&m=words' >/dev/null   # builds the index
kill %1

/tmp/sd-upstream-bin view -listen 127.0.0.1:38501 /tmp/sd-interop &    # must serve 200
/tmp/sd-upstream-bin convert -f html -o /tmp/sd-html /tmp/sd-interop -files=false
git worktree remove /tmp/sd-upstream --force
```

The claim being tested is the one in `README.md` and `view.md`: an archive this
fork has indexed still opens in stock slackdump. Nothing else in the suite
checks it, because nothing else can.

### 4. Watch for upstream schema migrations

If upstream added a goose migration under
`internal/chunk/backend/dbase/repository/migrations/`, re-read invariant 22
before assuming the sync is clean. The full-text index is deliberately **not**
a migration: goose errors on out-of-order migrations, so an index shipped as
one would make archives written by newer upstream builds unopenable by this
fork. `TestEnsureFTS/does not touch the goose version` is the guard.

### 5. Watch for new upstream workflows

```bash
gh api repos/battika/slackdump/actions/workflows --jq '.workflows[] | "\(.state)\t\(.name)"'
```

Any new workflow needing secrets this fork does not have will fail on every
push. Disable it server-side rather than deleting the file:

```bash
gh workflow disable <file>.yml --repo battika/slackdump
```

Address workflows by **filename or numeric ID, never display name**.
`gh workflow list` shows only active workflows unless given `--all`, and the
name lookup used by `disable`/`enable` searches that same filtered list — so
running it against an already-disabled workflow reports `could not find any
workflows named X` rather than "already disabled". Use `gh workflow list --all`
to see true state.

Currently disabled this way: **dickerhub** (pushes to upstream's Docker Hub
namespace with secrets this fork lacks) and **Docker Build**. `Go` and
`Shellcheck` are left active and are useful — `Go` runs the test suite on both
Linux and Windows.

### 6. Browser-validate the viewer

Go tests do not cover HTMX swaps, out-of-band refreshes, or anything about how
the page actually behaves. If the merge touched `internal/viewer/`, drive it in
a real browser with `playwright-cli`, run from `/tmp` rather than the repo.

**Point the viewer at a copy of an archive, never at a real backup.** The
viewer opens SQLite read-write and will build its full-text index inside
whatever you give it. Copy the archive **directory** (not just the `.sqlite`
file — attachment storage is only wired up when a directory is passed, and a
lone database file 404s every attachment and makes the console unreadable).

## Cutting a release

### Tag scheme

`vX.Y.Z-viewer.N`, where `vX.Y.Z` is the upstream release this build sits on
and `N` counts fork releases against that base. `v4.4.4-viewer.1`, then
`-viewer.2` for another fork-only change; when upstream ships v4.4.5, sync and
start again at `v4.4.5-viewer.1`.

Semver reads anything after the hyphen as a pre-release of the version it is
built from, which is backwards — these tags are upstream *plus* features. That
costs nothing in practice, but `.goreleaser.yaml` sets `release.prerelease:
false` so GitHub does not withhold the "Latest" badge. Do not remove it.

`vX.Y.Z+viewer.N` looks more correct and is worse: build metadata is ignored
for precedence, so `+viewer.1` and `+viewer.2` compare equal, and `+` becomes
`%2B` in every download URL.

### Procedure

```bash
# Park the release workflow while seeding the upstream base tag, or pushing it
# would cut a release of plain upstream.  Filename, not display name -- see the
# gh lookup quirk above.
gh workflow list --all --repo battika/slackdump      # confirm current state
gh workflow disable release.yml --repo battika/slackdump
git push origin vX.Y.Z
gh workflow enable release.yml --repo battika/slackdump

git tag -a vX.Y.Z-viewer.N -m "Viewer enhancements on upstream vX.Y.Z"
git push origin vX.Y.Z-viewer.N

gh run watch --repo battika/slackdump
gh release view vX.Y.Z-viewer.N --repo battika/slackdump
```

**Never run `git push --tags`.** The local clone carries 121 upstream tags, all
matching the workflow's `v*` trigger; pushing them arms a release run for every
one.

Seeding the upstream base tag first is what keeps release notes readable:
goreleaser derives them from commits since the previous tag, so with
`vX.Y.Z` present the range is exactly this fork's work plus any upstream
commits after that release. Without it, the notes start at the first commit in
project history.

### What gets built

Six artifacts, from `.goreleaser.yaml`: windows, darwin and linux on amd64 and
arm64. Windows ships `.zip`, the rest `.tar.gz`, each carrying the binary,
`LICENSE`, `README.md` and `slackdump.1`, plus `checksums.txt`.

`CGO_ENABLED=0` is load-bearing and must stay. It is also why search survives
cross-compilation: `modernc.org/sqlite` is pure Go, so FTS5 works in the
released binaries with no cgo toolchain. Verify a config change locally before
tagging:

```bash
goreleaser check
goreleaser release --snapshot --clean   # writes to dist/, which is gitignored
```

Binaries are unsigned. macOS Gatekeeper quarantines them (`xattr -d
com.apple.quarantine slackdump`) and Windows SmartScreen warns. Upstream has
the same limitation; changing it needs an Apple Developer ID and a Windows
code-signing certificate.

### GitHub cost

None. `GITHUB_TOKEN` is minted per workflow run — nothing to create or store —
and Actions minutes, release storage and download bandwidth are all free for
public repositories.

## Things that stay out of the repo

Design documents and implementation plans are never committed, in any path.
They go to the session scratchpad. Durable rationale belongs in the relevant
`AGENTS.md` instead, where it stays next to the code it constrains rather than
going stale in a plan file.
