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

Initial capture drivers are `mariadb`, `mongodb`, and `redis`. Planned later
drivers include `files` and `garage`. Initial destination drivers are `local`
and `s3`.

Driver configuration is namespaced below its target. Unknown fields must be
rejected so a misspelled safety option cannot silently fall back to a default.

Native commands must be launched with `exec.CommandContext`-style argument
arrays, never through a shell. Credentials use protected files, environment, or
native secure config mechanisms; they never appear in argv.

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
