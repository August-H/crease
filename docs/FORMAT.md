# Crease version 1 formats

These formats are experimental. A reader must reject unsupported versions rather than interpret them as version 1.

## Object identity

An object's ID is the lowercase hexadecimal SHA-256 digest of:

```text
crease 1 TYPE LENGTH<NUL>PAYLOAD
```

`TYPE` is `blob`, `tree`, or `commit`. `LENGTH` is the payload's byte length expressed in decimal. There is one ASCII space between header fields, no leading zeros in the length, and one zero byte immediately before the payload. IDs include type and length; a blob and a tree with the same payload have different IDs.

Blob payloads contain arbitrary bytes. Tree and commit payloads use the exact JSON encoding produced by Go's `encoding/json.Marshal` over the structs in `internal/model`, without indentation, trailing newline, or unknown fields. Struct field order is part of the encoding. Arrays are ordered; nil arrays encode as `null`, and empty arrays encode as `[]`. Both forms are accepted for empty arrays and have distinct IDs. Writers use the model constructors and preserve the resulting bytes. Readers decode, re-encode, and compare exact payload bytes to reject noncanonical encodings.

Trees have an `entries` array. Each entry has `name`, `mode`, and `id`, in that order. Entries sort by Go string byte order on name. Supported modes are `file`, `exec`, and `tree`. Names cannot include path separators, control characters, internal `.crease` or `.git` components, Windows device names, trailing spaces or dots, or case-colliding siblings. Sibling collision detection uses NFC normalization and Unicode case folding, while stored names retain their original bytes. UTF-8 must be valid. Tree depth is limited to 100.

Commits have `tree`, `parents`, `author`, `time`, and `message`, in that order. Parent order matters. There are zero, one, or two parents. Writers emit UTC RFC3339Nano timestamps. Authorship is descriptive metadata, not an authenticated signature.

Every referenced object must have the expected type. Before publishing a branch, the server validates new graph closures and reuses repository-scoped proofs it previously generated for unchanged closures. Clients cannot submit proofs. Limits include 8 MiB per payload and two million visited objects per validation operation.

## Physical storage

New objects live in `objects/packs/NAME.pack` with a matching `NAME.idx`. `NAME` is the lowercase SHA-256 of the complete pack bytes. The six-byte pack magic is `CRPK\x00\x01`, followed by binary records with no per-record length prefix. Each record's header includes its stored payload length.

The index begins with six bytes `CRIX\x00\x01`, then a four-byte unsigned big-endian record count. Each 45-byte entry contains the raw object ID in 32 bytes, an eight-byte unsigned big-endian pack offset, a four-byte unsigned big-endian record length, and a one-byte object type. Entries sort by ID. A 32-byte SHA-256 checksum covers all preceding index bytes. Readers check the checksum, bounds, object type, and corresponding pack file before using an index.

Writers accumulate objects until 4,096 records or roughly 16 MiB of raw and encoded memory, or an explicit publication boundary. A flush writes and syncs the pack, renames it, syncs its directory, then atomically publishes a synced index. Objects become visible to reopened readers through that index. A crash before index publication can leave an unreferenced pack, which readers ignore. References and checkout journals publish only after all required objects are durable. Unreferenced packs and delta bases are retained; no compaction or garbage collection runs.

For compatibility, readers also accept older loose objects under `objects/ID[0:2]/ID[2:]` as JSON with `type` and base64 `data` fields. New writes use packs. Hash identity and canonical payload encoding have not changed, so no history rewrite is required. Older binaries cannot read packed repositories. Back up before upgrading; physical format compatibility does not follow from RPC fallback.

Decoded metadata and indexes are cached for a command or server repository lifetime. First payload decoding checks canonical encoding and SHA-256 identity. Blob ingestion checks the full reconstructed contents. Existing immutable pack entries and server-generated closure proofs avoid repeated hashing during publication. `fsck` bypasses decoded caches to verify physical records and their delta bases.

SQLite databases use schema version 1, WAL journaling, FULL synchronization, foreign-key enforcement, and a five-second busy timeout. Additive tables store local fetched-history frontiers and server graph proofs. Each process serializes database access through one connection. Database transactions never span network transfers. A separate OS advisory lock excludes concurrent processes from the same workspace or server directory.

Schema definitions are identified by SHA-256 in `schema_definitions` and applied once per definition. Workspace tracking insert/delete triggers advance a generation counter in the same transaction. That counter invalidates cached tracked-path lists, including after writes by an older client.

## Disposable local indexes

These indexes are hints, written by atomic replacement without durability flushes. Missing, corrupt, stale, or unsupported indexes cause reconstruction from authoritative data. Object and reference publication never relies on a cache write succeeding.

`objects/pack-index` consolidates pack lookups. Its six-byte magic is `CRMI\x00\x01`, followed by four-byte pack and object counts. A manifest contains each covered pack's raw 32-byte name. Sorted 77-byte rows contain an object ID, pack name, eight-byte offset, four-byte record length, and one-byte type. A trailing SHA-256 covers all preceding bytes. Readers verify the manifest against current index names and read newer per-pack indexes as an overlay. Consolidation runs after 32 additional packs, avoiding a complete rewrite on every commit. No physical packs or delta bases are removed.

`.crease/workspace-index` begins with `CRWI\x00\x01` and ends with a SHA-256 checksum. It contains the tracking generation, cached base commit and snapshot, tracked paths, file fingerprints and digests, discovered paths, sparse selection, and monitor cursor. Integers and collection counts are eight-byte unsigned big-endian values. Strings have two-byte lengths. File entries use a one-byte file/exec mode and a raw 32-byte ID. Cache entries also carry a deletion flag. The base commit must match the current branch before its snapshot is reused; the tracked list must match the SQLite generation.

Workspace indexes load lazily, so history and review commands do not load every file fingerprint. Old SQLite `file_cache` entries remain readable for initial cache reconstruction. Inspection may cache an edited file's digest without storing its blob. A commit must persist those bytes before publishing a tree that references them. Unchanged files reuse objects from the durable committed snapshot.

## Local filesystem monitor

Monitoring is optional and defaults off. A helper uses kqueue on macOS or inotify on Linux and holds a separate workspace monitor lock. `.crease/monitor.json` contains an authenticated loopback endpoint and a random 256-bit access token, with owner-only permissions. This local protocol is independent of repository SSH RPC.

Each helper has a random epoch and a monotonically increasing event sequence. Before answering a change query, it drains the native queue and refreshes watches for changed paths. Clients acknowledge a sequence only after updating their file and discovery caches. Events arriving during that work remain after the acknowledged sequence. Restart, event loss, unsupported filesystem types, lost watches, or an unavailable helper require a full scan. The journal changes epoch after its record or byte budget is exceeded. Ignore changes refresh discovery; tracking-generation or sparse changes invalidate the workspace cursor.

The macOS backend accepts APFS and HFS and holds a descriptor per watched path. Linux accepts ext, XFS, Btrfs, tmpfs, ramfs, and overlay filesystems and watches directories. Both enforce watch limits and exclude internal metadata directories. Other filesystem types and resource-limit failures cannot silently enable cached status without reliable watches.

Full file scans open shared parent directories once, use descriptor-relative no-follow operations, and revalidate directory identities before returning a snapshot. This prevents a replacement symlink from redirecting a cached parent lookup outside the workspace.

## Binary object records

Packs and negotiated binary streams use the same record encoding. All integers are unsigned and big-endian.

| Offset | Bytes | Meaning |
| --- | ---: | --- |
| 0 | 32 | Raw SHA-256 object ID |
| 32 | 1 | Type: 1 blob, 2 tree, 3 commit |
| 33 | 1 | Encoding: 0 raw, 1 DEFLATE, 2 DEFLATE blob delta |
| 34 | 4 | Reconstructed payload length |
| 38 | 4 | Stored payload length |
| 42 | 32 | Delta base object ID, zero for other encodings |
| 74 | variable | Stored payload |

DEFLATE uses the raw stream format from Go's `compress/flate`. The current writer uses `BestSpeed` and keeps uncompressed bytes when compression does not save enough space. The physical encoding does not affect object IDs.

A delta decompresses to a four-byte common-prefix length, a four-byte common-suffix length, and replacement middle bytes. Reconstruction concatenates the base's prefix, the middle bytes, and the base's suffix. Both lengths must fit the base without overlapping, and the final length must match the header. Only blobs can use deltas. There are at most eight stored delta links. A wire record may refer to a base the receiver already holds; import reconstructs and validates it, then stores a full record if another delta would exceed that limit.

Records cannot exceed 74 + 8 MiB + 8 bytes. Decompression, patch bounds, reconstructed length, canonical model encoding, and the final object hash are checked independently.

## SSH and RPC

The server supports public-key authentication. Each accepted connection is identified by the authenticated key's SHA-256 SSH fingerprint. Every request checks the repository grant again, so revocation applies to subsequent requests on existing connections.

Only an SSH session executing the literal command `crease-rpc-v1` is accepted. Shells, arbitrary exec commands, SFTP, and forwarding are rejected. There is at most one RPC session per connection.

Each message contains a four-byte unsigned big-endian JSON byte length followed by that many bytes of JSON. Lengths must be greater than zero and at most 16 MiB. Unknown JSON fields and trailing JSON values are rejected. Requests and responses include `version: 1`. Request and response types are defined in `internal/protocol`.

The request contains an operation and repository name, plus operation-specific fields. Responses contain either operation results or an `error` string. Legacy JSON object batches contain at most 128 objects and at most 8 MiB of raw contents. A legacy get response may contain fewer objects than requested to stay within the byte limit; the client retries the remainder. There are two-minute connection deadlines during RPC and a 30-second server handshake deadline.

The authenticated `capabilities` operation advertises `binary-stream-v1`, `history-frontiers-v1`, and `graph-proofs-v1`. Clients fall back to ordinary JSON operations when an older peer rejects capability discovery. Ordinary responses omit extension fields, so older clients can still use a new server.

`fetch_stream`, `get_stream`, and `put_stream` begin with a normal request and a JSON response containing `stream: true`. Next come binary records, each prefixed by a four-byte length. A zero length ends the records. The sender then receives or sends a final framed JSON response carrying success or an error. An unexpected EOF is a failed transfer, not a clean terminator. Deadlines renew per record.

Binary uploads allow at most 4,096 objects and 64 MiB of reconstructed payload bytes per request. Acknowledgement follows a durable flush. Explicit downloads request at most 128 IDs per stream, optionally with target-to-base delta hints. History streams send the reachable metadata graph, plus historical blobs when `full` is requested, without a request for each generation. Total history size remains subject to graph traversal limits; each record has its own size bound.

Repository access is checked before object reads or writes. Object storage is scoped to a server-generated repository ID, and client repository names are never used as filesystem paths.

## Synchronization and review

A normal push transfers missing immutable objects before requesting a branch update. The update includes the expected old commit and proposed new commit. The server requires a complete graph and fast-forward ancestry, then compares the expected value inside the publication transaction. A retry of an already-applied update succeeds without another branch movement.

Clones retrieve commits and trees for one requested branch. Historical blobs are deferred unless full history is requested. Current selected files are fetched separately. Partial clients can enumerate object IDs from tree metadata without possessing historical blobs.

Clients record fetched commit frontiers only after a successful, durable transfer. Metadata and full-history completeness are recorded separately. Later history streams stop at up to 128 matching frontiers. Changing the configured remote clears these records and remote refs. For pushes, `common` accepts up to 128 candidate commit IDs and returns only those with this repository's persisted server proofs. The client stops inventory at those commits and skips unchanged subtrees from their snapshots. New closure proofs publish transactionally with branch updates or merge candidates, after object durability. The first validation of an older repository without proofs walks its graph to establish them.

Pull requests have stable numeric IDs and recorded source revisions. Approvals bind to exact source commit IDs. Comments bind to the revision supplied by the commenter. Creation and merge requests carry idempotency tokens, persisted by the CLI until the response succeeds.

Merge jobs capture the source revision. Candidate commits preserve the target and source as ordered parents. Required CI reports bind to the job ID and exact candidate ID. The publication transaction rechecks the source, target, approvals, current permissions, and required reports. Target changes replace candidates, so reports for an earlier candidate cannot authorize a later one.

Workspace checkout uses a flushed journal containing the previous visible snapshot, target visible snapshot, full logical snapshot, and final branch metadata. Recovery completes the operation. If a path's current contents match neither the old nor target version, recovery stops so a user can preserve intervening edits.
