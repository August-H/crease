# Git-style CLI

Crease uses familiar Git commands and terminal layouts for the supported operations below. It retains its own storage, SSH protocol, and commit behavior. Standard Git clients cannot use a Crease server.

## Commit behavior

`crease add` tracks files. `crease commit` includes every tracked change and deletion at commit time. There is no staging area, and editing a file after `add` includes those later edits in the commit. `commit -a` is accepted and has the same behavior as a normal Crease commit.

Long status groups tracked changes under `Changes to be committed` and explains this behavior. Short status shows `A `, `M `, or `D ` for tracked changes and `??` for untracked files. The first column means the change is eligible for the next Crease commit. It does not represent a saved staging snapshot.

## Common commands

| Command | Behavior |
| --- | --- |
| `crease init [-b main] [directory]` | Initialize locally; reinitializing an existing repository preserves its history. |
| `crease add main.go` | Track a file. |
| `crease add -A` | Track all nonignored files in the workspace. |
| `crease add -- -filename` | Track a filename beginning with a dash. |
| `crease rm --cached main.go` | Stop tracking a path while preserving its working file. |
| `crease status` | Show the branch, tracked changes, and untracked files. |
| `crease status -sb` | Show the branch and short status. |
| `crease status --porcelain=v1 -z` | Emit stable, NUL-terminated status with raw paths. |
| `crease commit -m "Message"` | Commit all tracked changes and show a summary. |
| `crease commit -am "Message"` | Same behavior, with familiar combined flags. |
| `crease log --oneline -n 10` | Show abbreviated commit IDs and subjects. |
| `crease log --decorate` | Include the current HEAD/branch decoration. |
| `crease show [HEAD]` | Show a commit and its patch against its first parent. |
| `crease diff` | Compare the current commit with tracked working files. |
| `crease diff HEAD~1 HEAD` | Compare two revisions. |
| `crease diff --stat` | Show file and line counts. |
| `crease diff --name-only` | Show changed paths. |
| `crease diff --name-status` | Show change codes and paths. |
| `crease diff --exit-code` | Exit with 1 when differences exist, without a fatal message. |
| `crease branch` / `crease branch -v` | List branches, optionally with commit IDs and subjects. |
| `crease branch feature` | Create a branch without switching. |
| `crease switch -c feature` | Create a branch and switch to it. |
| `crease checkout -b feature` | Alias for creating and switching branches. |
| `crease switch main` / `crease checkout main` | Switch to a local or published branch. |
| `crease branch -m [old] new` | Rename a branch. |
| `crease branch -d feature` | Delete a branch merged into the current branch. |
| `crease branch -D feature` | Delete an unmerged branch explicitly. The current branch cannot be deleted. |
| `crease remote` / `crease remote -v` | List `origin`, optionally with its URLs. |
| `crease remote add origin URL` | Record the address of an existing server repository. |
| `crease remote set-url origin URL` | Change that address. |
| `crease remote get-url origin` | Print the address. |
| `crease remote remove origin` | Remove the address, tracking settings, and cached remote references. |
| `crease push -u origin main` | Push the current branch and record its upstream. |
| `crease fetch origin main` | Fetch metadata and selected files without changing working files. |
| `crease pull --ff-only origin main` | Fetch and fast-forward the current branch. |
| `crease merge origin/feature` | Merge a published branch. |
| `crease config user.name "Your Name"` | Set the repository-local author name. |
| `crease config user.email "you@example.com"` | Set the repository-local author email. |
| `crease config --get user.name` | Read a setting. |
| `crease rev-parse --short HEAD` | Resolve a commit ID. |
| `crease rev-parse --show-toplevel` | Print the workspace root. |
| `crease -C /path/to/project status` | Run a command in another directory. |
| `crease help status` / `crease status --help` | Show command usage. |
| `crease --version` | Print the version. |
| `crease key init` | Create and select an Ed25519 identity once, without overwriting existing keys. |
| `crease key use /path/to/private-key` | Save an existing identity as the default. |
| `crease key share` | Print and copy the public key; print it even when no clipboard is available. |
| `crease key show` | Print only the public key, suitable for piping. |
| `crease key copy` | Copy only the public key to the system clipboard. |
| `crease key fingerprint` | Print the selected public key's SHA-256 fingerprint. |
| `crease access grant --role write` | Paste a teammate's public key and press Enter. |
| `crease access grant --clipboard --role write` | Grant access using a public key already copied to the clipboard. |
| `crease access grant - --role write` | Read one public key from stdin. |

The same public-key input forms work with `access revoke`. `access list` shows granted roles and key fingerprints. When no role is supplied, grants default to `read`. Explicit `--key`, then `CREASE_KEY`, take precedence over the identity saved by `key use` or `key init`. Encrypted private keys use the matching identity loaded in `ssh-agent`. `key init` generates an unencrypted private key protected by filesystem permissions; use `ssh-keygen` and `key use` when you want to generate a passphrase-protected key.

`create URL .` is a Crease extension for creating a server repository and connecting the current initialized workspace. Creating a remote does not upload commits; use `push` afterward.

## Output examples

```text
$ crease status
On branch main

Changes to be committed:
  (Crease commits all tracked changes; add tracks new files.)

        modified:   main.go

$ crease commit -m "Update game loop"
[main a1b2c3d] Update game loop
 1 file changed, 2 insertions(+), 1 deletion(-)

$ crease push origin main
To ssh://localhost:2222/snakegame
   123abcd..a1b2c3d  main -> main

$ crease push
Everything up-to-date
```

Commit summaries use actual changed-file and line counts, with create/delete/mode records. If older blob contents are deferred in a sparse or lazy clone, the summary can omit line counts; completing an offline commit never requires downloading old contents just for presentation. Diff output uses `diff --git`, standard file modes, `/dev/null` for added/deleted files, context hunks, and C-style path quoting. These are patch-format conventions; object IDs still refer to Crease objects.

Short status paths are always relative to the repository root. Normal output quotes whitespace and unusual bytes. `-z` emits raw paths. `--porcelain=v2` is not supported. Upstream status compares the local head with the last fetched or pushed remote reference, without contacting the server.

## Supported subset and legacy commands

There is one remote, named `origin`. Push and pull operate on the checked-out branch; switch branches before requesting another one. Pull always requires a fast-forward. Force-push, rebase, stash, tags, path-specific checkout/restore, and staged/index commands are not implemented. Unsupported flags fail rather than changing those semantics. Log follows first-parent history and defaults to 20 entries; `-n` accepts up to 10,000. Full commit IDs, unambiguous reachable cached abbreviations of at least four hex digits, `HEAD~N`, `HEAD^N`, `origin/BRANCH`, and `FETCH_HEAD` are supported.

The existing `branch create NAME [REF]` command still creates **and switches**, while Git-style `branch NAME [REF]` only creates. `remote set URL`, `fetch BRANCH`, `config author NAME`, `untrack PATH`, and `remote/BRANCH` references remain supported. `restore [REF] --force` retains Crease's existing whole-workspace restore behavior. The PR, access-control, monitor, backup, and recovery commands remain available through `crease help`.
