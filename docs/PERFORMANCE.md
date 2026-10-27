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

## Long-history and review stress suite

The completed 200-commit, 200-PR run passed all correctness checks and executed 5,025 setup and CLI commands in 192.5 seconds. Each tool created 601 commits including the initial, feature and merge commits. The Crease server retained 200 merged PRs, 200 comments, 200 approvals, 200 completed jobs and 201 branch references. [The generated report](benchmarks/stress-200.md) contains the full comparison, and [the JSON](benchmarks/stress-200.json) preserves all samples and parameters.

| Operation | Crease median | Git median |
| --- | ---: | ---: |
| Clean status after 200 main commits | 11.404 ms | 7.230 ms |
| Working diff | 9.671 ms | 5.079 ms |
| Incremental commit including Git staging | 32.118 ms | 14.459 ms |
| Incremental SSH push | 51.828 ms | 81.214 ms |
| SSH clone with current files | 8,649.595 ms | 181.039 ms |

The clone row compares Crease's lazy historical contents with Git's full reachable history. Git's separate depth-1 clone took 127.773 ms. Crease's clone is the largest measured latency gap despite its smaller historical-content scope. Profiling transfer and durable working-file materialization is the next step; this suite does not attribute the gap to a specific function. Crease status stayed near 11–13 ms across the history checkpoints, and listing 50 reviews at 200 open PRs took 8.392 ms. All 200 Crease merges completed in 5.880 seconds with four submitting clients, including CLI polling. This does not establish merge-worker capacity independent of the clients.

Crease server storage was 14.449 MiB with 428 packs, including review data. Git used 11.755 MiB before explicit garbage collection and 0.834 MiB afterward. The separately timed `git gc` took 424.226 ms. Crease currently has no corresponding compaction. Git won most everyday command measurements; the faster incremental-push result does not establish an overall speed advantage for Crease.

`make stress` builds Crease and runs `scripts/stress.py`. Its default workload contains 1,000 text files, a 256 KiB seeded random binary, a synthetic `.env` and a file whose executable permission changes. Each tool receives an initial commit, 200 main commits, 200 feature commits and 200 merge commits. Main-history edits include individual text changes, scattered binary changes, additions, deletions and mode changes.

The suite starts two temporary SSH listeners on loopback. Crease uses its actual server implementation. Git uses a benchmark-only Go SSH adapter that runs the installed `git-upload-pack` and `git-receive-pack` binaries against one disposable bare repository. The adapter accepts one generated identity and two exact commands, provides no shell, and is not a Git hosting product. Host keys are pinned before clients connect. All keys, repositories, processes and host-trust files are isolated from existing services and cleaned up afterward.

Four independent workspaces per tool publish branches concurrently. Crease also creates 200 actual PRs, adds a comment to each, requests each diff, approves each head and submits all merges through its single merge worker. Review pagination is checked at 0, 50, 100, 150 and 200 open PRs. Git publishes equivalent branches and merges each locally before an SSH push. Git does not implement PRs, permissions, approval policy or a merge queue, so the report treats those workflows separately.

The test fails on command errors, missed dirty files, empty diffs, mismatched file contents or executable modes, incorrect history lengths, incomplete PR pagination, lost comments, unsuccessful merges or invalid final clones. After each measured clone, it compares the initial revision with the first edit and verifies the expected original and replacement lines. This exercises historical blob reads without printing a full initial patch, which can exceed the CLI's 4 MiB diff-output limit for large fixtures. After graceful server shutdown, it checks the persistent PR, comment, approval and job counts, confirms all jobs and PRs were merged, and runs SQLite integrity checking. Both tools run `fsck` on fresh final clones. Crease's `fsck` permits historical blobs that its lazy clone has not fetched; a successful result does not mean every historical blob was downloaded and checked.

Reports include every timing sample, median, nearest-rank p95 and maximum; clean status at successive history sizes; ordinary and shallow SSH clone timings; concurrent feature pushes; review lifecycle timings; and metadata sizes and pack counts. Initial operations and final clone/fsck operations have one sample. Other sample counts are recorded explicitly. Storage includes logical bytes and allocated filesystem blocks. Git automatic maintenance is disabled during command timings, and a separately measured `git gc` shows the compacted server size. Crease has no equivalent compaction yet. Server storage totals include Crease's review metadata, which Git does not have.

Crease's normal clone defers historical file contents. Git's normal clone retrieves full reachable history, and its separate depth-1 clone omits older history entirely. Neither is an identical retrieval policy. The report labels all three rather than treating clone latency as an equal-work comparison. Commands include process startup and SSH setup, use warm OS caches with monitors disabled, and retain each tool's default durability settings. Measurements do not include WAN delay, memory, CPU or wire-byte accounting. The Git SSH adapter differs from a production OpenSSH service, and one machine running clients and servers does not establish deployed capacity.

```sh
make stress

# Print terminal histograms from a saved run without starting servers.
python3 scripts/stress.py --report docs/benchmarks/stress-200.json

# Short correctness trial, including an addition and its subsequent deletion.
python3 scripts/stress.py --files 20 --commits 21 --prs 8 \
  --workers 4 --runs 2 --output /tmp/crease-stress-smoke.json

# Larger history and review population.
python3 scripts/stress.py --files 1000 --commits 500 --prs 500 \
  --workers 4 --runs 7 --output docs/benchmarks/stress-500.json
```

Go, Git, OpenSSH and Python 3.9 or later are required to run benchmarks. `--seed` makes the fixture repeatable, and `--binary` selects a different Crease build. The suite creates an adjacent Markdown report for each JSON output and prints hey-style latency histograms, fastest/slowest/average timings, and nearest-rank percentiles for each operation and tool. Bucket labels are inclusive upper bounds aligned to whole-millisecond steps, favoring 5 ms and 10 ms increments and using larger round steps for wider ranges; bars encode sample counts, with the most populated bucket scaled to at most 40 characters. Histograms stay separate for each operation and tool. Output adapts to terminal width and uses ASCII bars when the output encoding cannot represent square characters. `--report JSON` displays saved results using only Python, without rebuilding Crease or running benchmarks. A failed run exits nonzero, cleans up its resources and does not publish a successful report.

## Clone pipeline and profiling

The completed rerun uses the same 1,000-file, 200-main-commit, 200-feature and 200-merge fixture as the original stress test. All correctness checks passed, including 200 reviewed PRs and 5,025 setup/CLI commands. Raw data and all other command measurements are in [clone-optimized-200.json](benchmarks/clone-optimized-200.json) and [the generated report](benchmarks/clone-optimized-200.md). The original results remain in [stress-200.json](benchmarks/stress-200.json).

| SSH clone | Previous median | Current median | Current p95 |
| --- | ---: | ---: | ---: |
| Crease, lazy historical contents | 8,649.595 ms | 456.139 ms | 480.450 ms |
| Git, full reachable history | 181.039 ms | 188.820 ms | 195.855 ms |
| Git, depth 1 | 127.773 ms | 119.277 ms | 127.513 ms |

Crease's median fell by 94.7%, about 19 times faster on this workload. Git's full clone remained about 2.4 times faster in the current run, while retrieving more historical contents. These are warm-cache measurements on the same host, not WAN or cold-cache guarantees. Current median phase times were 62.004 ms for metadata reception, 53.041 ms for file reception overlapping checkout preparation, 19.453 ms for the final file-sync batch, and 249.155 ms for publication. Publication includes canonical object flushing, reference/frontier persistence, the journal, working-path links, directory syncing and final branch metadata; the timer alone does not attribute it to one function. The combined transfer used one request after capability discovery, with eight checkout workers. The full suite completed in 93.8 seconds, compared with 192.5 seconds previously.

`crease clone URL [directory] --timings` emits JSON phase timings on stderr without changing ordinary clone output. With `clone-stream-v1`, one request after capability discovery pins the branch and returns its metadata followed by selected current blobs. Each blob is validated during ingestion, then its decoded bytes feed checkout directly. File preparation overlaps reception through up to eight workers and a 64 MiB budget for active and queued checkout payloads. This budget excludes graph metadata, encoded input and object-store buffering.

The initial parallel implementation still interleaved tiny file writes with durability barriers. Its 1,000-file, 21-commit trial took a median 3,642 ms, with 3,212 ms in the file-reception interval including checkout backpressure. Preparing the complete batch before the parallel file-sync pass reduced a subsequent one-commit trial to 410 ms. The latter is a single sample with a different history length, not the final before/after comparison.

The batch retains a sync for every working file. It then flushes canonical objects and publishes the existing recovery journal. Working paths are linked without replacing existing files, using anchored parent descriptors; directory identities are checked before metadata publication. Each affected directory is synced once, with children before parents, before the final reference transaction and journal removal. Distinct paths with identical blobs have independent working inodes. A failed or truncated stream never publishes a working file or branch. Interrupted publication recovers from canonical objects and protects intervening edits.

The phase report includes authentication/capability discovery, workspace initialization, metadata reception, preparation, file reception, remaining checkout wait, file sync, publication, staging cleanup and total time. The checkout-pipeline interval overlaps reception and includes its final sync; timings must not be added together. Encoded byte counters cover application records and their framing, excluding SSH overhead. `scripts/stress.py` requests timings for every measured Crease clone and preserves them under `clone_phases` in its JSON output, with a table for the ordinary clone samples.

New clients fall back to fetch when the server does not advertise the extension, while retaining batched checkout for fresh workspaces. Both endpoints need the new build to use combined transfer and overlap downloading with file preparation. Ordinary branch switches, pulls and recovery continue using their established journal path. Integration tests exercise sparse/full clones, more than 128 current blobs in one request, old binary/JSON fallbacks, missing or duplicate contents, bad hashes, metadata ordering, pinned-head mismatch, interrupted publication, untracked collisions and symlink replacement.

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

Small commits pay durable pack/index and SQLite publication costs. Each commit creates another pack; consolidated lookup indexes do not compact physical packs. There is no compaction or garbage collection. Clone publication now accounts for more time than the file-sync batch on this fixture; further instrumentation can separate its directory, journal and database work. Watch registration and directory rescans also need measurements under large rename and build workloads.

Blob delta matching only finds a common prefix and suffix. Block matching, large-file chunking, and support above the 8 MiB object limit remain future work. Fresh clones batch working-file preparation and syncing, but larger trees still need measurements of file creation, directory publication and tracking updates.

The stress suite measures warm end-to-end clones and concurrent clients on one host. Cold clones, Git partial-clone comparisons, longer-lived pack populations, realistic WAN conditions and heavier merge-queue contention still need controlled measurements. Do not extrapolate these results into a hosting capacity claim.
