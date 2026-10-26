# Performance measurements

On the 10,000-file workload, the local-command changes reduced monitor-free status from 147 ms to 66 ms, diff from 256 ms to 54 ms, and incremental commit from 193 ms to 79 ms. With Crease's optional native monitor enabled, the latest measurements were 22 ms, 29 ms, and 53 ms. Git remains faster for the measured monitor-free everyday commands; monitored status was similar between the tools. There is no demonstrated overall speed advantage over Git.

## Local commands without monitors

Measured October 26, 2026, on an Apple M4 running macOS 15.7.8, with Go 1.26.4 and Git 2.54.0. Fixtures contain small text files in nested directories and ten subsequent commits changing one file. Warm status and history have seven samples; diff and incremental commit have ten. Initial tracking and commit are single observations.

The previous column is the packed-storage implementation before the latest workspace changes. Its raw results remain in [packed-1000.json](benchmarks/packed-1000.json) and [packed-10000.json](benchmarks/packed-10000.json). Current raw results are [incremental-1000.json](benchmarks/incremental-1000.json) and [incremental-10000.json](benchmarks/incremental-10000.json). Git columns come from the current runs.

### 1,000 files

| Operation | Previous Crease | Current Crease | Current Git |
| --- | ---: | ---: | ---: |
| Warm status, median | 21.888 ms | 12.323 ms | 6.478 ms |
| Diff after one file changes, median | 44.871 ms | 10.689 ms | 5.303 ms |
| Commit one changed file including Git staging, median | 41.706 ms | 33.931 ms | 18.285 ms |
| Initial tracking and first commit, total | 77.300 ms | 62.153 ms | 338.718 ms |
| Warm history display, median | 6.109 ms | 5.654 ms | 4.154 ms |
| Repository metadata | 977,658 bytes | 1,083,832 bytes | 500,995 bytes |

### 10,000 files

| Operation | Previous Crease | Current Crease | Current Git |
| --- | ---: | ---: | ---: |
| Warm status, median | 147.041 ms | 66.357 ms | 15.516 ms |
| Diff after one file changes, median | 256.213 ms | 54.130 ms | 11.326 ms |
| Commit one changed file including Git staging, median | 193.253 ms | 79.081 ms | 39.161 ms |
| Initial tracking and first commit, total | 485.744 ms | 435.559 ms | 3,472.578 ms |
| Warm history display, median | 12.606 ms | 8.060 ms | 4.238 ms |
| Repository metadata | 5,426,490 bytes | 5,926,185 bytes | 2,465,160 bytes |

The workspace and consolidated object indexes trade some disk space for less decoding, SQL work, and repeated file opens. Current metadata is larger than the previous packed-store measurement. The original loose-object 1,000-file baseline remains in [local-1000.json](benchmarks/local-1000.json); its initial tracking and commit took 8,904 ms.

## Both native monitors enabled

This profile enables `crease monitor start` and Git's built-in monitor through `core.fsmonitor=true` and `git fsmonitor--daemon start`. Helpers start after the initial commit. The first status establishes a cache baseline before the warm samples. Both helpers stop before fixture cleanup. Raw results are [monitored-1000.json](benchmarks/monitored-1000.json) and [monitored-10000.json](benchmarks/monitored-10000.json).

| Operation, 10,000 files | Crease | Git |
| --- | ---: | ---: |
| Warm status, median | 21.919 ms | 22.488 ms |
| Diff after one file changes, median | 28.604 ms | 15.825 ms |
| Commit including Git staging, median | 52.653 ms | 41.275 ms |
| Helper startup, separate from command samples | 158.049 ms | 7.781 ms |

Monitor benefits depend on repository size and workload. IPC and helper startup add overhead, especially for small repositories. Compare the two tools within the same profile. These samples report foreground latency, not total CPU or resident memory across the CLI and helper. macOS native monitoring was exercised by integration tests and these benchmarks; the Linux implementation was cross-compiled but has no runtime measurement here.

## Method and reproduction

The benchmark disables Git's global and system configuration to avoid hooks, signing, and machine-specific behavior. It enables filesystem monitors only when the corresponding flags are supplied. It does not enable additional Git large-repository tuning. Crease commits tracked edits directly; Git updates its index, so the workflow rows include staging. Raw results also record Git's commit command separately.

Both tools wait 1.1 seconds before warm status samples. Crease's full scan conservatively rehashes recently modified files. Commands run as separate processes with warm OS caches and recently generated contents already cached. Crease retains fsynced object packs, durable reference publication, and SQLite FULL synchronization. Git's durability settings are not customized.

Results exclude SSH, network transfer, merges, CI, and cold-machine startup. The fixture makes small in-place edits; mass renames and directory creation may require broader monitor rescans. They are synthetic measurements on one host, not production latency or capacity guarantees. The 100,000-file case has not been measured.

```sh
make build
python3 scripts/benchmark.py \
  --files 1000 --history 10 --runs 7 \
  --output docs/benchmarks/incremental-1000.json
python3 scripts/benchmark.py \
  --files 10000 --history 10 --runs 7 \
  --output docs/benchmarks/incremental-10000.json
python3 scripts/benchmark.py \
  --files 10000 --history 10 --runs 7 --monitor --git-fsmonitor \
  --output docs/benchmarks/monitored-10000.json
```

## SSH history transfer

These earlier measurements predate these local-command optimizations and have not been rerun.

The integration benchmark fetches a branch with 50 commits into a new workspace through a real SSH listener on loopback. It compares negotiated binary streaming against forced legacy JSON RPC on the same client, server, and packed object store used in that run. It includes workspace initialization, SSH setup, history metadata, current file retrieval, and object persistence. It does not materialize working files or simulate WAN latency.

| Transfer mode | Median of three runs | Application requests |
| --- | ---: | ---: |
| Binary stream | 30.709 ms | 3 |
| Legacy JSON | 884.879 ms | 53 |

Each run transfers the same history and current file. Binary records total 22,626 bytes, including record length prefixes, excluding JSON control messages and SSH framing. JSON wire bytes are not instrumented, so there is no measured wire-size ratio. The timings reflect both fewer requests and fewer durable flush boundaries. This is a comparison with the legacy Crease path, not with Git clone. Raw output is in [history-transfer.txt](benchmarks/history-transfer.txt).

```sh
go test ./internal/syncer -run '^$' \
  -bench BenchmarkHistoryTransfer -benchtime=1x -count=3
```

A separate integration test changes a few bytes in a 1 MiB random file after both peers have its baseline. Push and fetch each transfer less than 4 KiB of binary object records, including the new commit and tree. The test verifies reconstructed contents from disk. This demonstrates the implemented prefix/suffix delta case; dispersed edits can require much larger deltas or a full object.

## Listing reviews

The existing server microbenchmark populates 10,000 pull-request metadata rows, then requests a page of 50 reviews through the application handler. It measured 68,618 ns per operation, about 0.069 ms, with 38,024 allocated bytes per operation. The output is in [review-10000.txt](benchmarks/review-10000.txt). This older measurement was not rerun for the storage changes.

```sh
go test ./internal/server -run '^$' \
  -bench BenchmarkReviewList10000 -benchmem -count 1
```

This measures an indexed, warm database listing and permission check. It excludes SSH authentication, wire encoding, network time, realistic review bodies, diff generation, and concurrent merge work. It does not establish end-to-end throughput for a deployed server. Descriptions and diffs are fetched separately.

## Implemented changes

- Inspection hashes edited files without storing temporary blobs or flushing object packs. Commits persist inspected bytes before publishing references.
- Commit-to-commit diffs avoid working files, compare directory object IDs before descending, and fetch only changed contents. Mode-only changes need no content reads. Working diffs reuse the cached committed snapshot.
- Incremental commits rewrite only affected ancestor trees. Tests compare the result with a complete rebuild, including additions, deletions, executable changes, and empty-directory pruning.
- File scans share anchored directory descriptors and reject symlink substitutions and directory-identity changes. They retain conservative fingerprint checks when a full scan is required.
- Compact workspace indexes cache tracked paths, fingerprints, snapshots, discovery, and monitor cursors. SQL triggers invalidate tracking generations. Consolidated binary object indexes cover older packs with a small overlay for newer ones. Corrupt hints rebuild from authoritative storage.
- The optional native monitor drains pending events before serving queries, refreshes affected watches, and uses epoch/sequence cursors. Restarts, lost watches, overflow, changed selections, unsupported filesystems, and unavailable helpers force full scans. Changed ignore rules refresh discovery.
- Bounded Myers line comparison replaces the quadratic matrix. Common ends are trimmed first. Diffs show three lines of context. Tests reconstruct random inputs and apply a 10,000-line text patch with Git when available. The frontier search stops after 2,048 edit steps or 16 million work units; excessive text changes fall back to full-replacement diff output, while ambiguous merges remain manual.
- Packed object writes, negotiated binary history streams, validated history frontiers, DEFLATE compression, and eight-link blob deltas remain in place. Duplicate payload validation during local writes was removed; the storage encoder still checks each new payload before it can be published.

## Remaining performance work

Warm monitored commands still decode a workspace index and compare path maps, so their CPU and memory work scales with tracked path count even when filesystem checks do not. Further profiling could guide a resident workspace index, more targeted index reads, and cheaper process startup. Wide changed directories still require rewriting all their direct entries.

Small commits pay durable pack/index and SQLite publication costs. Each commit creates another pack; consolidated lookup indexes do not compact physical packs. There is no compaction or garbage collection. Watch registration and directory rescans also need measurements under large rename and build workloads.

Blob delta matching only finds a common prefix and suffix. Block matching, large-file chunking, and support above the 8 MiB object limit remain future work. Working-file materialization still performs durable individual writes and can dominate a large clone.

Cold and warm end-to-end clones, Git partial-clone comparisons, concurrent clients, long-lived pack counts, realistic WAN conditions, and merge-queue contention need controlled benchmarks. Do not extrapolate these results into a hosting capacity claim.
