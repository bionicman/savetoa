# Architecture

## Product boundary

SaveToA is a command-line orchestrator. It coordinates native database tools,
packages their output, applies optional transformations, and delivers immutable
artifacts to one or more destinations. It is not a database server, scheduler,
repository daemon, filesystem snapshotter, or replacement for native backup and
restore utilities.

Systemd owns scheduling, mutual exclusion at the unit level, resource limits,
and failure supervision. Configuration management owns package installation,
target definitions, credential files, schedules, host permissions, and alerts.

Optional lifecycle hooks are a narrow event adapter, not embedded monitoring.
SaveToA discovers root-managed executables under a fixed directory and invokes
them directly with a non-secret versioned JSON event on stdin. Target YAML
cannot select an executable, supply shell text, or template arguments. Hook
failure never rewrites the already established action outcome.

## Domain model

A **target** is a named backup job. It contains exactly one capture driver and
the settings required to execute that job. A driver is not a target: the same
driver may serve many targets with different sources, credentials, policies,
and destinations.

A **group** is an ordered collection of target names for operator convenience.
It does not merge their consistency boundaries.

A **backup set** is the immutable result of one successful target execution.
It has a unique ID, payload, versioned manifest, and completion marker.

## Planned pipeline

```text
target
  -> validate configuration
  -> acquire target lock
  -> source health gate
  -> capture driver
  -> source-specific validation
  -> package and compress
  -> optional client-side encryption
  -> durable local spool
  -> fan out to destinations
  -> verify destination acknowledgements
  -> write completion marker last
```

Capture and delivery must be separable. If an offsite destination is
temporarily unavailable after a valid local artifact exists, a later retry must
deliver the same backup ID rather than capture a different database state.
Recovery from S3 performs the reverse storage flow: it reads the completion
marker first, validates the exact manifest it binds, streams the expected
payload into a private local partial set, and publishes that set only after its
size and checksum match. Restore continues to consume only the local-store
contract.

## Locking and spool

Every target run acquires a cross-process advisory lock named only from its
validated target name. Lock acquisition is context-cancellable. Different
targets may run concurrently; the same target may not.

Capture first stages one completed, verified backup set in the durable spool.
Delivery streams that staged payload to a destination while requiring the
recorded size and checksum to match. A retry reuses the same path, backup ID,
manifest, and payload. If an identical completed set is already present, local
delivery succeeds idempotently after verification. A different set with the
same ID is a conflict and is never replaced.

When enabled, X25519 `age` encryption is streamed before spool staging. The
spool and every destination therefore store and checksum the same ciphertext;
delivery retries never re-encrypt or create a new artifact for the backup ID.

## Driver interfaces

Implemented capture drivers are `mariadb`, `mongodb`, `redis`, `sqlite3`, and `tar`.
Planned later drivers include `garage`. Initial destination drivers are
`local` and `s3`.

S3 endpoints require HTTPS. Plain HTTP is accepted only for an origin whose
host is the loopback-only `localhost`, `127.0.0.0/8`, or `::1`; this supports a
co-located S3-compatible service without permitting cleartext remote storage.

Driver configuration is namespaced below its target. Unknown fields must be
rejected so a misspelled safety option cannot silently fall back to a default.

Native commands must be launched with `exec.CommandContext`-style argument
arrays, never through a shell. Credentials use protected files, environment, or
native secure config mechanisms; they never appear in argv.

MongoDB capture connects directly to the configured local member. Its health
gate requires the expected replica-set name, healthy SECONDARY state, bounded
optime lag behind a healthy PRIMARY, and a hidden, non-voting, priority-zero
member configuration. The full `mongodump --archive --oplog` output is wrapped
as `dump.archive` inside the ordinary payload tar, so storage transformations
and destinations remain driver-independent.

Redis capture connects only to the configured replica and requires the
expected upstream, an up and fully synchronized replication link, bounded
last-I/O lag, read-only mode, and promotion priority zero. It verifies the
effective RDB path, requests `BGSAVE SCHEDULE`, waits for a newer successful
`LASTSAVE`, and copies only the atomically published regular RDB into the
ordinary artifact pipeline.

Tar capture archives an explicit, bounded list of absolute filesystem paths
with the system GNU tar. The executable and arguments are fixed by SaveToA;
configuration cannot supply tar flags or shell text. Configured roots may not
overlap or be symlinks, special files are rejected, and a symlink inside a
selected tree may only resolve within another selected tree. Stored member
names are relative to `/`. The completed uncompressed tar is staged in the
private work directory before the common zstd, age, spool, and destination
pipeline begins.

SQLite capture opens one explicit database through the native `/usr/bin/sqlite3`
CLI in read-only, no-follow mode. It runs a fixed `PRAGMA quick_check` before
capture, uses the CLI's online backup command to create `database.sqlite3` in a
private work directory, normalizes the snapshot to rollback-journal mode, and
checks the standalone database read-only before packaging it.
The output path and SQLite commands are fixed by SaveToA; configuration cannot
supply SQL, CLI flags, or executable paths. The online backup incorporates
committed WAL state into the standalone snapshot, so WAL and SHM sidecars are
not copied as separate artifacts.

## Artifact format

The first format version uses a unique prefix with this layout:

```text
<environment>/<target>/<year>/<month>/<day>/<timestamp>-<random-id>/
  payload.tar.zst[.age]
  manifest.json
  complete
```

The exact manifest schema is defined in [Manifest format v1](manifest-v1.md).
`complete` is written last. Listing, retention, and restore ignore sets without
a valid completion marker.

The local writer creates each backup ID directory exclusively and never reuses
or replaces an existing directory. It creates a private partial directory,
creates and syncs every file, and writes the completion marker last. It then
publishes the directory with a dirfd-relative rename and syncs the parent. A
failed attempt is removed when possible; a crash may leave a hidden partial
directory, but it cannot leave a published incomplete set.

The manifest records target, capture driver, format version, timestamps,
source/tool versions, non-secret replication metadata, sizes, transformation
metadata, and ciphertext checksum. It must contain enough information to select
the restore driver without consulting the current target configuration.

## Restore boundary

`restore` materializes into an explicit destination. It must not stop a live
service, replace a datadir, run `copy-back`, replay an oplog into a production
server, or activate a restored service unless a future separately designed
operator contract makes that destructive boundary unmistakable.

Restore verification against disposable services is a first-class feature, not
an optional documentation exercise.

Tar restores use the ordinary `restore` command. Safe relative symlinks,
permissions, modification times, and numeric ownership when restoring as root
are preserved. Absolute paths, traversal, escaping symlinks, hard links, device
nodes, FIFOs, sockets, duplicate members, and extraction through symlinked
parents are rejected before a partial target can be published.

MongoDB restore verification does not accept an operator-supplied database
endpoint. It materializes into a new explicit directory, starts a temporary
loopback-only `mongod` with a new dbpath there, replays the embedded archive and
oplog, stops the process, and reports success only after clean shutdown.

Redis restore verification likewise accepts no service endpoint. It starts a
temporary loopback-only `redis-server` against the materialized RDB with AOF
and automatic saves disabled, waits for a successful load and PING, then stops
the process cleanly.

## Retention boundary

Retention is target-scoped and runs under the same cross-process target lock as
capture, delivery, and fetch. It first discovers and validates completed sets
in the spool and every configured destination, then builds every repository's
plan before deleting anything. An invalid completion marker or manifest aborts
the entire planning phase. Published directories and S3 prefixes without a
valid completion marker are ignored.

Daily buckets use UTC calendar dates, weekly buckets use ISO weeks, and monthly
buckets use UTC calendar months. The newest completed set in each configured
bucket is retained; the union of daily, weekly, and monthly selections wins.
Each repository is planned independently because delivery history may differ.

Deletion removes the completion marker first and then only the payload and
manifest named by the revalidated set. A crash or remote error can therefore
leave harmless incomplete remnants, but cannot leave a partially deleted set
visible as complete. Unknown local files and unrelated S3 objects are never
recursively removed.
