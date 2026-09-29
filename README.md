`git-cow-worktree` is a drop-in replacement for `git worktree add`.

It uses copy-on-write for the worktree, reducing disk usage.

It is entirely vibe-coded, but there's also not that much to it. Use at your own risk.

Everything below is LLM-written.

---

## Install

```sh
go install github.com/josharian/git-cow-worktree@latest
```

Git can dispatch `git cow-worktree ...` when the installed `git-cow-worktree` binary is on your `PATH`.

## Usage

Install the beta pack, then use the `sq` entry point:

```sh
sq packs add git-cow-worktree
sq git-cow-worktree add [git-worktree-add flags] [--from <path>] [-v] <path> [<commit-ish>]
```

Each distinct repository, target path, and argument set is assigned deterministically to the stock or CoW arm with 50:50 probability. Retrying the same operation stays in the same arm. `sq` records the command's standard telemetry, including total duration, exit status, and command arguments, plus bounded `arm:stock|cow`, `backend:stock|cow|unknown`, and `result:success|failure` labels. Paths and branch names are never included in custom labels, but may be present in `sq`'s standard argument telemetry.

Installing the companion `git-cow-worktree-beta` agent skill opts an agent into creating worktrees through this command. Remove the skill to opt out of agent-driven beta use.

The public binary remains usable directly as a drop-in Git command; direct invocations retain its existing behavior and do not join or report the experiment:

```sh
git cow-worktree add [git-worktree-add flags] [--cow=auto|off] [--from <path>] [-v] <path> [<commit-ish>]
```

Examples:

```sh
git cow-worktree add ../repo-feature feature
git cow-worktree add -b topic ../repo-topic main
git cow-worktree add -v --from ../repo-main ../repo-topic topic
```

Most flags are inherited from `git worktree add`.

Added by `git-cow-worktree`:

- `--cow=auto`: preflight the source/target pair and use CoW only when supported; otherwise invoke stock `git worktree add`. This mode is explicit and is not yet the default.
- `--cow=off`: invoke stock `git worktree add` directly.
- `--from <path>`: use a specific source worktree instead of auto-selecting one.
- `-v`, `--verbose`: print the chosen source, clone counts, and timings.

Special case:

- `--no-checkout`: passed through to `git worktree add`; no reflinking is attempted.

## Filesystem Support

Reflinking is supported on APFS on macOS and on Linux filesystems with `FICLONE` support, such as btrfs, XFS, and bcachefs. On unsupported filesystems or across devices, `git-cow-worktree` falls back to Git's normal checkout behavior.

## Scope

`git-cow-worktree` only replaces `git worktree add`. Other `git worktree` subcommands should be run with Git directly.

## How it works

It first runs `git worktree add --no-checkout`, so Git creates the new worktree metadata but leaves the working tree empty.

Then it picks a source worktree. Candidates are the current worktree, the main worktree, and a few recently modified other worktrees. It ignores the target worktree and unmaterialized worktrees. Candidates are scored by `git rev-list --left-right --count <source>...<target>`; fewer commits ahead/behind is better.

It runs `git ls-tree -r -t` on the source and target commits and plans the cheapest way to materialize each path. A directory whose tree SHA is the same on both sides has identical tracked contents, so where the filesystem supports it (APFS does; Linux reflinks are file-only) the whole directory is cloned in a single syscall. Otherwise it descends and reflinks matching files one by one. Paths that are dirty, untracked, ignored, or submodules in the source are skipped, along with symlinks and mode mismatches.

Every cloned file is then hashed and compared against the blob it is supposed to be, and the ones that check out are written to the new worktree's index with their stat data. This is the step that preserves the sharing: Git decides whether to rewrite a working-tree file by comparing it against its index entry, and a worktree with no index — which is what `--no-checkout` leaves behind — makes checkout rewrite every file, undoing every clone. Hashing in parallel is also substantially cheaper than letting Git refresh the index itself, which is single-threaded and uses a collision-detecting SHA-1.

Finally it runs Git checkout in the new worktree. That fills in files that were not cloned, overwrites anything stale, and produces the same index a plain checkout would. Every step before it is best-effort: if reflinks are unsupported, if the source can't be inspected, or if the index can't be written, the checkout still runs and just becomes a normal one.

## Canary safety boundaries

The optimized path creates the worktree in a private staging directory, disables hooks while populating it, validates the requested `HEAD` and clean status, then publishes it with `git worktree move`. The `post-checkout` hook runs once at the final path. A hook failure is returned without rolling back the published worktree, matching native Git behavior.

Paths with filters, working-tree encoding, ident, or text/EOL conversion attributes are excluded from CoW seeding and are populated by Git. Submodule paths are likewise never cloned from a source worktree. Handled failures before publication remove transaction-owned worktree metadata and compare-and-swap-delete only branches created by the operation.

Crash recovery and telemetry orchestration belong to the embedding canary wrapper. This binary provides structured phase boundaries in code but does not upload telemetry.
