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

The first format version is expected to use a unique prefix resembling:

```text
<environment>/<target>/<year>/<month>/<day>/<timestamp>-<random-id>/
  payload.tar.zst[.age]
  manifest.json
  complete
```

The exact schema remains to be implemented and tested before compatibility is
promised. `complete` is written last. Listing, retention, and restore ignore
sets without a valid completion marker.

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
