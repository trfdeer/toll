---
name: Create Release
description: Use when cutting a toll release: preflight checks, choosing and bumping the version, writing release notes, tagging, and verifying the published GHCR image. Also covers what version the binary and container image report.
---

# Create a toll release

Releases are a git tag plus a Nix-built OCI image. CI does the publishing; the
tag is the trigger. The only thing a human usually edits is the version prefix
in the Nix files.

Read `references/versioning.md` before touching versions — it explains why the
reported version is not simply the git tag.

## Workflow

### 1. Preflight

Releases are cut from `main` on a clean, tested tree:

```sh
git switch main
git pull --ff-only
git status --porcelain          # must be empty
go test ./...
go vet ./...
```

If the release includes web changes, also run `cd web && bun run typecheck`.
There is no separate test workflow, so these local checks are the gate.
Optionally confirm the latest `main` snapshot build is green first:

```sh
gh run list --workflow=release.yml --branch main --limit 1
```

**Signing is mandatory.** The global git config signs commits and tags over SSH
(`commit.gpgsign` / `tag.gpgsign` = true, `gpg.format = ssh`,
`gpg.ssh.program = /opt/1Password/op-ssh-sign`). Unlock 1Password before you
start. If signing fails, fix the 1Password/SSH setup and retry — never bypass it
with `--no-sign` or a config override.

Do not tag from a dirty tree: Nix bakes the version at build time and a dirty
tree falls back to `0.1.0-dev` (see `references/versioning.md`).

### 2. Choose the version

The version is **hardcoded** in the Nix files — Nix cannot see git tags — so a
human still sets it. git-cliff derives the SemVer bump from the commits since
the last tag, using the `[bump]` section in `cliff.toml`:

```sh
git-cliff --bumped-version             # suggests the version, e.g. v0.2.0
git tag --list --sort=-v:refname       # previous releases
grep -rn '0\.1\.0' nix/                # current hardcoded prefix, 3 spots
```

`--bumped-version` only reads tags and commits; it writes nothing. Policy for
this repo (pre-1.0):

- `fix` / `refactor` / `perf` → **patch** (`0.1.0` → `0.1.1`)
- `feat` → **minor** (`0.1.0` → `0.2.0`)
- breaking change (`feat!`, `BREAKING CHANGE:`) → **minor** while on `0.x`
  (`breaking_always_bump_major = false`); going to `1.0.0` is a deliberate
  decision, not automatic
- `chore` / `ci` / `docs` / `style` / `test` / `build` → **no bump**; a range
  containing only these suggests no release is needed at all

Confirm the suggestion matches intent (it is a suggestion, not a commit). Then
update the three `"0.1.0"` literals in `nix/packages.nix` and `nix/web.nix` to
the target version — see `references/versioning.md` for the exact lines. If the
target already equals the prefix, nothing changes here.

**First release:** there are no tags, so `--bumped-version` returns the
`initial_tag` (`v0.1.0`). That already matches the hardcoded prefix, so step 5
is a no-op and you go straight from notes to tagging.

### 3. Generate release notes

Notes are generated from the authored commit bodies by **git-cliff**
(`cliff.toml` at the repo root). Conventional-commit parsing reads each
`Added` / `Changed` / `Deprecated` / `Removed` / `Fixed` / `Security` heading as
a footer, and `cliff.toml` merges those sections across the release's commits in
canonical order:

```sh
git-cliff --unreleased --tag vX.Y.Z --strip header > /tmp/toll-release-notes.md
```

`--tag` only labels the release in the output; it does **not** create a git tag.
Review the file before using it. Non-conventional commits are filtered out, and
`docs` / `ci` commits have no body, so they contribute nothing.

git-cliff ships in the devshell (`nix develop`); outside it, prefix the commands
with `nix run nixpkgs#git-cliff --`.

Optionally maintain a committed full changelog as well:

```sh
git-cliff -o CHANGELOG.md   # then commit it in step 5
```

### 4. Verify the version

```sh
./.opencode/skills/create-release/scripts/report-versions.sh
```

On a clean tree this should print, for each of `.#toll`, `.#web` and `.#image`,
`<chosen-version>-<shortRev>` (e.g. `0.1.0-096c0c1`; the node_modules package
uses `+` instead). `plain go` will still say `dev` — that is expected.

Optionally prove the real binary:

```sh
nix build .#toll && ./result/bin/toll --version
```

### 5. Commit the bump (only if you changed a prefix)

If you bumped a prefix, only the Nix files changed, so the scope is `nix`
(add a regenerated `CHANGELOG.md` on the same commit if you keep one):

```sh
git add nix/packages.nix nix/web.nix
git commit -m "chore(nix): bump version to X.Y.Z"
git push
```

Never commit a tag before the bump lands on `main`; CI builds from the tagged
commit.

### 6. Tag and push

```sh
git tag -a vX.Y.Z -F /tmp/toll-release-notes.md   # git tag rejects -m and -F together
git push origin vX.Y.Z
```

The tag name must match `v*` (`.github/workflows/release.yml`). CI then builds
`nix build .#image` and pushes:

- `ghcr.io/trfdeer/toll:vX.Y.Z` — the tag name verbatim
- `ghcr.io/trfdeer/toll:latest` — moved only on tag pushes

Images are **linux/amd64 only** (deliberate; not a gap to fix).

### 7. Watch and verify the publication

```sh
gh run list --workflow=release.yml --limit 1
gh run watch
```

Once it succeeds:

```sh
docker pull ghcr.io/trfdeer/toll:vX.Y.Z
docker run --rm ghcr.io/trfdeer/toll:vX.Y.Z --version   # -> toll version X.Y.Z-<sha>
```

Confirm `:latest` points at the same image, then optionally publish the notes
(git-cliff generated them in step 3):

```sh
gh release create vX.Y.Z --title "toll X.Y.Z" --notes-file /tmp/toll-release-notes.md
```

## If a step fails

- **Signing fails** — stop and fix the 1Password / SSH setup, then re-run the
  failed `git commit` or `git tag`. Never fall back to an unsigned commit or tag.
- **The tag push or workflow fails** — the tag exists but no image was
  published. Fix the cause on `main`, push it, wait for the snapshot build, then
  move the tag onto the fix: `git push origin :vX.Y.Z`, `git tag -d vX.Y.Z`,
  re-tag and push. A failed run never moves `:latest`.
- **The notes are wrong** — edit `/tmp/toll-release-notes.md` and re-tag. If the
  tag was already pushed and consumed, do not move it: cut a new patch version.

## Do not

- Do not create unsigned commits or tags, or disable signing to get past a
  failure.
- Do not tag a version whose prefix was not bumped in the same commit — the
  binary would report the old `x.y.z` with only the hash changed.
- Do not tag from a dirty tree or a commit that is not on `main`.
- Do not expect `go build`, `go install …@vX.Y.Z`, or `web/package.json` to carry
  the version; they do not (see `references/versioning.md`).
- Do not edit an already-published tag to "fix" its version; bump and re-tag.
- Do not hand-write the changelog sections: git-cliff derives them from the
  commit-body footers. Fix the notes file (step 3) before tagging, and keep
  `cliff.toml`'s section list in the order defined by `AGENTS.md`.