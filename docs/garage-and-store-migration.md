# Garage metadata and store migration: design boundary

This is a design, not an implemented configuration or command contract. No
Garage capture or store migration command is currently accepted by SaveToA.
The two workflows below have different consistency and recovery guarantees and
must not be presented as one backup driver.
The current manifest parser reserves the historical name `garage`, although
the target parser rejects it. Before implementation, settle the manifest
identifier (`garage-metadata` below), its provenance fields, and compatibility
handling; do not publish `garage` backup sets under an ambiguous contract.

## Garage node metadata backup

`garage-metadata` is a future capture driver for one Garage node's database
snapshot. It belongs in the existing target pipeline: source health and
snapshot gates, private capture, optional transforms, durable spool, immutable
local/S3 delivery, and ordinary materialization into a new explicit directory.
Its backup set is **not** a backup of object data or a portable clone of a
different Garage cluster.

Garage's own snapshot operation is the consistency boundary. The driver must
request a fresh snapshot with a fixed native Garage invocation, then identify
and copy only the snapshot produced for that request. It must not tar a live
`metadata_dir`, select an arbitrarily old automatic snapshot, or follow a
symlink out of the configured snapshot directory. Garage can place snapshots
under `metadata_snapshots_dir`; the database may be LMDB, SQLite, or another
supported engine, so the driver must use a version-tested allowlist of snapshot
layouts rather than assume one file name. Snapshot creation and capture need
bounded time and space; a failed or ambiguous snapshot must fail closed.

The source contract should name a local Garage configuration reference,
expected database engine, and exact snapshot directory. An installation must
grant only the access needed to invoke snapshot and read its result; package
installation grants none. A full node-recovery set needs the Garage-produced
database snapshot plus a verified copy of the node identity and cluster layout
sidecars; the live database and Garage configuration file are not copied.
Sidecars must be read with no-follow path checks and must not change across
capture. The private `node_key` makes this a secret-bearing artifact, so this
driver must require age encryption before publishing to the spool or any
destination. Native output is not copied into errors or logs. A future
manifest extension should record the Garage version, database engine, and
non-secret snapshot identity/time, but never RPC secrets, access keys, raw
configuration, or node-private-key bytes.

`restore` only materializes the captured bundle into a new directory. It
does not stop Garage, replace a database, assign a node, change a cluster
layout, or claim that object blocks are present. A disposable restore test
must start an isolated Garage instance with an explicitly compatible version
and architecture, and verify that the snapshot can be read without contacting
production peers. An isolated copy of the same node identity must never run
on a production network. LMDB metadata is not portable across architectures.
Restoring a production node remains an operator procedure with its own safety
review. Garage documents that recovery from a metadata snapshot requires
stopping the node and can lose changes since the snapshot on an unreplicated
store ([configuration](https://garagehq.deuxfleurs.fr/documentation/reference-manual/configuration/),
[recovery](https://garagehq.deuxfleurs.fr/documentation/operations/recovering/)).

The opt-in `scripts/smoke-garage-metadata.sh` validates the pinned v2.3.0 LMDB
case in two `--network none` containers. In that release, `garage meta
snapshot` produced `snapshots/<UTC timestamp>/db.lmdb` as a regular file,
whereas the live database used `db.lmdb/data.mdb`. Replaying that file as
`data.mdb` preserved a synthetic bucket only when the isolated restore also
had `node_key`, `node_key.pub`, and `cluster_layout`. A fresh node identity
could start but did not list the bucket. This is evidence for the recovery
bundle above, not proof of object-data recovery or compatibility with another
Garage release or database engine. The test's empty data directory did not
need Garage's `data_layout` or `garage-marker`; a procedure reusing real block
data must account for them separately.

## Generic store migration

Store migration is a separate, explicitly invoked workflow, not a capture
driver, SaveToA backup set, S3 destination, or retention job. Its first scope
is copying between explicit disk and S3-compatible locations: disk-to-disk,
disk-to-S3, and S3-to-S3. S3-to-S3 must stream through the runner; an S3
`CopyObject` request cannot be assumed to copy across independent providers.
No migration operation deletes from or changes the source. The destination
must be distinct, explicitly named, and empty or hold only byte-identical
objects from a matching resumable run. Conflicting destination content fails
closed; overwrite, mirror, and prune are outside this contract.

A migration run needs its own durable, versioned inventory and journal, not
the single-payload backup manifest. The inventory records source and
destination identities without credentials, exact bucket/key or relative disk
path, byte length, and a SHA-256 calculated from streamed source bytes. The
journal records each object only after destination bytes have been read back
and compared. ETag, object count, and a successful PUT alone are not proof of
content equality. A crash resumes from the journal and revalidates both
source and destination before skipping an object. Credentials for source and
destination are separate protected files; access-key values, URLs with
credentials, and native error bodies never enter argv, logs, or the journal.

There is no atomic snapshot of a live S3 bucket in this design. A normal copy
reports per-object verified progress, not a point-in-time whole-store backup.
For a completed migration, writes to the source must be quiesced by the
operator before final inventory and reconciliation, and remain quiesced until
the destination has been verified and the application cutover is explicitly
performed outside SaveToA. If writes cannot be quiesced, the run must not
report an exact migration. In particular, Garage currently does not support
S3 bucket versioning, so a listing cannot recover overwritten or deleted
versions ([S3 compatibility](https://garagehq.deuxfleurs.fr/documentation/reference-manual/s3-compatibility/)).

S3 object user metadata, content headers, tags, ACLs, and versions need an
explicit compatibility policy before an S3 migration implementation. The
first implementation must either preserve each advertised field and verify
it, or reject the object with an unsupported-feature error. It must never
silently turn a data-only copy into a claim of full-fidelity migration.
Disk endpoints must reject traversal, symlinks, special files, and destination
paths that overlap the source; the copy must not interpret object keys as
filesystem paths without a checked mapping.
Garage bucket/key administration, access grants, and application endpoint
switching are deployment work, not effects of the copy command.

## Implementation gates

1. Validate the pinned Garage release's snapshot command and output layout
   with a disposable local node (v2.3.0 LMDB probe complete); cover other
   intended engines, concurrent automatic snapshots, and snapshot cleanup
   races before adding `garage-metadata` to config v1.
2. Implement and test metadata capture and materialization through the common
   durable backup pipeline. A fresh snapshot, marker-last publication, invalid
   layout rejection, and isolated replay are required acceptance cases.
3. Specify the migration inventory schema and supported S3 metadata subset.
   Implement disk-to-disk first, then disk-to-S3 and S3-to-S3 using the same
   journal and read-back verification. Exercise conflicts, interrupted copy,
   changed source, and a final quiesced reconciliation against isolated stores.

No production Garage or object store is required for these design and initial
test increments.
