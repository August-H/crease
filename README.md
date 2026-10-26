# Crease

Private, self-hosted version control in Go. One binary provides a local CLI and an SSH repository server. Each collaborator uses their own SSH key. There is no hosted account or browser login.

Crease has its own object format and protocol. Standard Git clients cannot use its repositories. This is an initial implementation with explicit limits, not a drop-in Git replacement.

The CLI follows Git's common command syntax and output conventions. See the [command reference](docs/CLI.md) for supported flags and differences. Crease keeps its original commit behavior: `add` tracks files, and `commit` includes all tracked changes.

## Build

Requires Go 1.26 or newer. The current supported systems are Linux and macOS. SQLite is embedded through a pure Go driver; no database service or C toolchain is required.

```sh
go build -o bin/crease ./cmd/crease
./bin/crease help
```

Put the binary on your `PATH` to use the examples below. `make release` builds Linux and macOS binaries for amd64 and arm64 into `bin/releases/`. It does not publish them.

## Run a private server

Generate a client identity if you do not already have an SSH key:

```sh
ssh-keygen -t ed25519 -f ~/.ssh/crease
```

Copy the **public** file, `crease.pub`, to the server, along with the Crease binary. Keep the private file on your own machine.

```sh
crease serve \
  --data /var/lib/crease \
  --listen :2222 \
  --owner-key ./crease.pub
```

The data directory must be writable by the service user. The process prints its SSH host-key fingerprint. Verify that fingerprint when a client connects for the first time. Changing the bootstrap public-key file does not replace an existing server owner.

Run this process on a VPS, EC2 instance, or another host with persistent local storage. Allow inbound TCP traffic on the chosen port. Use the [Docker setup](deploy/DOCKER.md) or the sample systemd service in [deploy/crease.service](deploy/crease.service).

## Create and clone

On your machine:

```sh
export CREASE_KEY="$HOME/.ssh/crease"

crease create ssh://your-server:2222/my-project
cd my-project
crease config user.name "Your Name"
crease config user.email "you@example.com"

# Create files, then track and commit them.
crease add .
crease commit -m "Initial version"
crease push
```

On another machine with an authorized key:

```sh
crease clone ssh://your-server:2222/my-project
```

`create` makes the remote repository and a local workspace. `init [directory]` makes a local repository without a server. Add a remote afterward with `crease remote set URL`. Only the server owner creates remote repositories in this release.

To create a server repository for a project you have already initialized and committed locally, run these commands inside the project folder:

```sh
crease create ssh://your-server:2222/my-project .
crease push
```

The final `.` selects the current workspace. This preserves local commits and working files and configures the remote. `remote set` only records an address; it requires the server repository to exist already.

The client can use `ssh-agent`, an explicit key file, or a default unencrypted key in `~/.ssh`. Use an agent for passphrase-protected private keys. This release does not parse `~/.ssh/config` or provide an interactive private-key passphrase prompt.

Global options come before the command:

```sh
crease --key ./private-key --known-hosts ./known-hosts clone ssh://host:2222/project
```

Known host keys default to `~/.crease/known_hosts`. `CREASE_KNOWN_HOSTS` overrides that path. `--trust-host` or `CREASE_TRUST_HOST=1` accepts a previously unknown host without a prompt, for environments where the fingerprint has been verified separately. A changed known key is rejected even with that option.

Save an existing private-key path once with `crease key use /path/to/private-key`, then ordinary commands use that identity. Explicit `--key` and `CREASE_KEY` take precedence over the saved choice. Passphrase-protected identities use their matching key from `ssh-agent`; load it with `ssh-add /path/to/private-key`.

## Local workflow

For an existing project folder, initialize its local repository before adding files:

```sh
crease init
crease add main.go
crease commit -m "Initial version"
```

Run these commands inside the project folder. `create` and `clone` initialize their destination automatically. The SSH trust directory at `~/.crease` is client configuration and does not initialize repositories in other folders.

```sh
crease status
crease diff
crease switch -c feature/login
# Edit tracked files.
crease commit -m "Add login"
crease push
```

`branch NAME` creates a branch without switching. `switch -c NAME` and `checkout -b NAME` create and switch to a new branch; add a revision to choose its starting point. The original `branch create NAME [REF]` syntax still creates and switches. `switch NAME` switches to a local branch or fetches a published branch of that name.

`add` tracks files; there is no separate staging area. `commit` includes all tracked changes and deletions. New files need `add` first. `untrack PATH` keeps files on disk and removes them from the next snapshot. Commits, diffs, and history work offline when their required objects are cached.

Status and working-file diffs hash edits without writing temporary repository objects. Commits write the changed blobs and affected ancestor trees, reusing unchanged subtrees. Comparing two commits skips the working directory and downloads only changed file contents when needed. Text diffs show three lines of surrounding context.

`.creaseignore` supports comments, directory prefixes, and shell-style `*`, `?`, and character classes. Patterns without a slash match path components. Git-style negation and `**` semantics are not implemented. Ignore rules apply to untracked files, so adding an ignore pattern does not untrack existing files.

Internal `.crease` and `.git` directories are always excluded. `.env` files are allowed and can be tracked explicitly.

```sh
crease fetch
crease pull
```

`fetch` updates remote references without changing workspace files. `pull` only fast-forwards. If branches diverge, merge explicitly:

```sh
crease merge remote/main
```

For conflicting changes, Crease applies nonconflicting edits and leaves this branch's contents at conflicting paths. It prints the conflicting paths and stores both merge parents. Inspect the other revision with `crease diff HEAD remote/main`, edit the resolution, and run:

```sh
crease merge --continue
# Or discard the merge's tracked changes:
crease merge --abort
```

A normal `commit -m MESSAGE` also finishes a pending merge. Conflict resolution is explicit; Crease does not insert conflict markers. Conflicting paths outside a sparse selection are materialized for resolution.

`restore [REF]` restores tracked files without moving the branch. Add `--force` to discard tracked edits. Checkout operations protect untracked collisions. After an interrupted checkout, `recover` finishes the journaled operation and refuses to overwrite intervening edits.

## Optional filesystem monitor

Commands work without a background process. To reuse filesystem events between commands:

```sh
crease monitor start
crease monitor status
crease monitor stop
```

The helper uses the same binary and runs only for this workspace. `monitor run` keeps it in the foreground for a service manager. Its log is `.crease/monitor.log`.

The monitor uses kqueue on macOS and inotify on Linux. It drains pending events before answering each command. Restarts, event loss, tracking or sparse-selection changes force a full scan. Ignore changes invalidate untracked-file discovery. Commands also fall back to full scans whenever the helper is unavailable.

Monitoring supports APFS and HFS on macOS, and ext, XFS, Btrfs, tmpfs, ramfs, and overlay filesystems on Linux. It rejects other filesystem types, including network mounts. It watches at most 50,000 paths on macOS or directories on Linux. macOS needs an open descriptor per watched path; the helper raises its own soft limit within the existing hard limit when possible. Resource-limit failures leave commands using full scans. Ignored build directories can still consume watches; internal `.crease` and `.git` directories are excluded.

## Selective downloads

```sh
crease clone ssh://host:2222/project --paths services/api,shared
crease sparse set services/api shared
crease sparse clear
```

A clone retrieves commit and tree metadata for the requested branch, plus the selected current files. It defers older file contents. Changing a sparse workspace preserves unselected files in the logical snapshot. `sparse clear` restores the complete working tree.

Historical comparisons and restores download missing file contents as needed. To prepare all history for the current branch for offline work:

```sh
crease fetch --full
```

`fetch BRANCH` fetches another branch. `fetch --commit ID` fetches an exact commit, including a server-generated CI candidate. Clone does not retrieve review discussions or file contents for unrelated branches.

## Access and review

Each teammate creates an identity once, then shares their public key:

```sh
crease key init
crease key share
```

`key init` creates an unencrypted Ed25519 private key with mode `0600` at `~/.crease/id_ed25519`, writes its `.pub` file, and selects it as their default identity. It refuses to overwrite existing key files. To keep an existing identity, use `crease key use /path/to/private-key` instead. `key share` prints the public key and copies it to the clipboard when available. The private key stays on its owner's computer.

From inside the repository, the owner or an administrator can paste the received public key directly:

```sh
crease access grant --role write
# Paste the single public-key line and press Enter.
```

Or grant from the clipboard without creating a file:

```sh
crease access grant --clipboard --role write
```

Public-key files, quoted public keys, and piped input also work:

```sh
crease access grant ./teammate.pub --role write
crease access grant "ssh-ed25519 AAAA... teammate" --role write
cat ./teammate.pub | crease access grant - --role write
crease access list
crease access revoke ./teammate.pub
```

Clipboard support uses `pbcopy`/`pbpaste` on macOS or `wl-copy`/`wl-paste`, `xclip`, or `xsel` on Linux. Without a clipboard, `key share` still prints a shareable public key. `key show` prints only that key for piping, `key copy` requires a working clipboard, and `key fingerprint` prints its SHA-256 fingerprint. Pasted, piped, and clipboard input is validated before contacting the server; private keys, multiple keys, oversized input, and `authorized_keys` options are rejected. Access defaults to `read` when no role is specified.

The saved identity path lives in `~/.crease/identity.json`; it contains no private-key contents. `CREASE_HOME` can relocate the identity settings, generated key pair, and default known-hosts file for automation. Sharing keys or granting access does not make a localhost-only server reachable from another computer.

```sh

crease pr create --base main --title "Add login"
crease pr list --limit 50 --after 0
crease pr diff 1
crease pr comment 1 --body "Reviewed this revision"
crease pr approve 1
crease pr merge 1 --wait
```

Read access allows cloning, fetching, and reading reviews. Write access also permits publishing branches, reviewing, and requesting merges. Admin access manages grants and required CI checks. The server owner retains access to every repository.

Each merge requires at least one approval of the current source revision from a current writer. Self-approval is allowed in this release. Publishing another source revision makes previous approvals insufficient. Comments retain their original revision ID. `pr show ID --after COMMENT_ID --limit N` paginates comments.

Merge requests enter a persistent queue. `job ID` shows progress and the candidate commit ID. The worker resumes after restart. Normal pushes reject non-fast-forward and stale branch updates.

## Required CI checks

Administrators can require named checks for queued merges:

```sh
crease policy checks set tests lint
crease policy checks
crease pr merge 1
crease job 1
```

The job enters `checking` with a candidate commit. A CI worker with an authorized administrator SSH key can fetch and test that exact candidate:

```sh
crease fetch --commit "$CANDIDATE"
crease branch create ci-run "$CANDIDATE"

# Run your test and lint commands, then report their actual results.
crease check "$JOB" --name tests --head "$CANDIDATE" --status success
crease check "$JOB" --name lint --head "$CANDIDATE" --status success
```

Reports require admin access in this release. Available statuses are `pending`, `success`, and `failure`. `crease check JOB` lists reported results. Advancing the target regenerates the candidate and requires new results. A failed check fails the job; fix the problem and enqueue again. Remove requirements with `crease policy checks clear`.

Checks gate queued merges. Direct fast-forward pushes remain available to writers; protected-branch policies are not implemented. Crease provides reporting commands, not a hosted CI runner.

## Storage, privacy, and recovery

Objects are immutable and identified by SHA-256. New objects share durable binary packs, with compression and bounded blob deltas. SQLite stores local tracking state and server permissions, branches, reviews, checks, and jobs. Each repository has a separate object directory. SSH clients get repository RPC access without a server shell or forwarding.

Clients negotiate binary streaming and send a branch's history metadata in one request. Incremental synchronization stops at previously fetched history, and the server reuses its own persisted graph-validation results. An acknowledged branch update always follows durable object publication.

The server operator can read repository files, including committed secrets. SSH protects transport; storage is not automatically encrypted. Use encrypted storage and backups where needed. Removing access does not erase existing clones or old secrets from history.

Stop the server before backing up:

```sh
crease backup --data /var/lib/crease --output /backups/crease-2026-10-26
crease restore-server --input /backups/crease-2026-10-26 --data /var/lib/crease-restored
```

Backup and restore destinations must not exist. Backups include a consistent SQLite copy, object files, access rules, and the SSH host private key. Restoration verifies file hashes and preserves host identity. Protect backup directories like the live server.

`crease fsck` verifies cached objects reachable from the current branch and reports historical blobs not yet downloaded. This release retains unreferenced objects and records branch movements in SQLite. There is no automatic garbage collection or public reflog command yet.

## Current limits

- Files and individual objects are limited to 8 MiB. JSON frames are limited to 16 MiB. Binary uploads contain at most 4,096 records and 64 MiB of raw contents per request; downloads stream bounded records. Legacy JSON object batches contain at most 128 objects and 8 MiB of raw contents.
- Local commands use an exclusive workspace lock. The server runs one process with up to 64 SSH connections and one merge worker.
- Each repository supports up to 50,000 branches. Review lists are paginated and omit full descriptions; use `pr show` for those.
- Linux and macOS regular files and executable bits are supported. Symlinks, submodules, and file-to-directory transitions at the same path are unsupported.
- Text merging is conservative. Binary conflicts, ambiguous multiple merge bases, and comparisons exceeding the bounded Myers search need manual handling. Text diffs fall back to a full replacement when the search limit is reached, subject to the 4 MiB output limit.
- Packs use DEFLATE compression when it saves space. Blob deltas reuse a prior version's common prefix and suffix, with at most eight stored delta links. There is no arbitrary block matching, large-file chunking, repacking, or garbage collection.
- Status uses a compact persistent workspace index keyed by file identity, size, mode, modification time, and change time. Full scans rehash recent or changed files conservatively. The optional monitor avoids filesystem checks for unchanged paths; cache decoding and snapshot comparisons still scale with tracked path count.
- There is no web UI, Git import/export, history rewriting, signed-commit verification, automatic secret encryption, geographic replication, or high availability.
- The format and schema are versioned but experimental. This release reads older loose JSON objects and falls back to JSON RPC with older peers. Older binaries cannot read the new packs on disk. Back up repositories before upgrading, and upgrade both endpoints to use the streaming improvements.

## Development and measurements

```sh
go test -race -timeout 120s ./...
go vet ./...
go test ./internal/server -run '^$' -bench BenchmarkReviewList10000 -benchmem
python3 scripts/benchmark.py --files 1000 --history 10 --runs 7
```

The integration suite opens SSH and monitor listeners on loopback. It tests two clients, lazy history, sparse commits, permissions, revocation, stale pushes, stale approvals, CI candidates, conflict resolution, corruption, concurrent updates, and backup restoration. Storage and transfer tests cover interrupted pack publication, bounded delta chains, legacy RPC fallback, history frontiers, and large-file deltas on push and pull. Local tests cover native monitor events and fallback, cache corruption, symlink replacement, incremental tree equivalence, commit diffs with inaccessible working files, and large text patches validated by Git when available.

[docs/FORMAT.md](docs/FORMAT.md) specifies the wire and object formats. [docs/PERFORMANCE.md](docs/PERFORMANCE.md) describes the benchmark scope and results. [PLAN.md](PLAN.md) contains the original architecture and delivery plan.
