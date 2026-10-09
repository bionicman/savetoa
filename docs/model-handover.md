# Model handover

This document preserves the product decisions made before repository creation.
Future coding agents must treat it as architectural context, not as an
implementation claim.

## Identity

- Project and brand: **SaveToA**.
- Repository and Go module: `github.com/bionicman/savetoa`.
- Binary and Debian package: `savetoa`.
- The name refers to the former PC instruction to save to floppy drive `A:`.
- The project is independent from its first consumer and intended for public
  GitHub development.

## Product contract

- SaveToA is one extensible Go CLI, built as a static binary where practical.
- A target is a named job. The target configuration chooses its capture driver,
  source credentials/settings, transforms, destinations, and retention.
- A capture driver is not itself a target.
- Initial capture drivers: MariaDB, MongoDB, Redis.
- Initial destination drivers: local filesystem and S3-compatible object
  storage.
- Later source scope: filesystem/ACME state and Garage metadata/object flows.
- Scheduling and supervision belong to systemd, not an embedded daemon.
- Configuration management owns target files, secrets, host permissions,
  schedules, and alert wiring.
- Shell orchestration was rejected as the primary implementation because the
  workflow will grow into health checks, retries, manifests, fan-out,
  encryption, retention, and restores.

## Evaluated upstream tools

- Restic and Kopia are strong encrypted repositories but do not understand
  database replica health or native capture semantics. Restic also treats
  encryption as fundamental rather than a clean optional transform.
- Rclone is a transport, not a database backup orchestrator.
- WAL-G remains interesting as a future adapter/backend, but its MariaDB,
  MongoDB, and Redis surfaces are not uniformly stable and its builds are
  database-oriented.
- GoBackup was rejected after source inspection: command strings and secrets
  reach argv, MariaDB output is not prepared, Redis freshness semantics are
  inadequate, and its OpenSSL CBC contract is unsuitable.

Do not re-run this product selection from scratch without a new requirement or
material change in those projects.

## Capture requirements

### MariaDB

- Run against a healthy read-only replica.
- Validate replication source, both threads, GTID state, and bounded lag.
- Use matching native `mariadb-backup` and server versions.
- Capture with replica metadata and safe-replica behavior.
- Prepare the physical backup before declaring success.
- Preserve replication coordinates in the non-secret manifest.

### MongoDB

- Run against a healthy replica-set secondary.
- Validate expected replica-set identity, member state, and bounded lag.
- Use a full `mongodump --archive --oplog`; filtered database/collection dumps
  are incompatible with the required oplog consistency contract.
- Keep credentials out of URI arguments, preferably using a protected native
  config file.
- Restore verification uses `mongorestore --oplogReplay` on a disposable
  instance.

### Redis

- Validate replica source, link/sync state, lag, read-only policy, and
  non-promotable priority.
- Request `BGSAVE SCHEDULE`, wait for successful completion and a newer
  `LASTSAVE`, then copy the atomically published RDB.
- Use `REDISCLI_AUTH` or an equally non-argv mechanism.
- Record non-secret persistence and replication metadata.

## Storage and artifact decisions

- Local disk is a first-class destination, not merely a temporary step.
- S3-compatible storage is a first-class destination and initial offsite path.
- Capture and delivery are separable so failed S3 delivery can retry an
  existing durable artifact without recapturing data.
- Backup sets are immutable and uniquely named.
- Manifest and artifact formats are explicitly versioned.
- A completion marker is durable and written last.
- Incomplete sets are ignored by restore and retention.
- Checksums cover stored ciphertext when encryption is enabled.
- Retention/prune can use a separate storage-maintenance identity.
- S3 versioning/Object Lock are optional provider-level defenses.

## Encryption decision

- Encryption is optional at the product level.
- The preferred first driver is recipient-based `age` with dedicated X25519
  recipients.
- The backup runner should hold only public recipients where possible;
  decryption identities stay offline and are escrowed separately.
- S3 SSE is orthogonal to client-side encryption.
- Never accept secrets in CLI arguments.

## Packaging decision

- Deliver a conventional Debian package, not only a release binary.
- Package, source project, and binary are named `savetoa`.
- Default config: `/etc/savetoa/config.yml`.
- Secret references: `/etc/savetoa/credentials.d/`.
- Encryption recipients: `/etc/savetoa/recipients.d/`.
- Persistent state/work/spool: `/var/lib/savetoa/`.
- Default local destination: `/var/backups/savetoa/`.
- Runtime locks: `/run/savetoa/`.
- Logs use journald initially.
- Package creates an unprivileged `savetoa` account via sysusers and paths via
  tmpfiles/systemd directory directives.
- Package installation neither enables timer instances nor grants database
  group membership.
- Native database utilities are target-specific runtime prerequisites installed
  by the consumer, not hard dependencies dragging every database stack onto
  every SaveToA host.

## Safety and implementation rules

- Use Go process APIs with explicit argv arrays; never execute target config as
  shell.
- Use contexts, bounded timeouts, cancellation, and explicit process cleanup.
- Redact secret-bearing native-tool errors before reporting them.
- Do not silently accept unknown config.
- Do not let configuration select an arbitrary executable or execution user.
- Never overwrite a live datadir in the ordinary restore command.
- Tests must exercise partial files, interrupted upload, native-tool failure,
  corrupt manifests, wrong encryption identity, path traversal, retention, and
  secret redaction.

## Immediate next iteration

Do not implement all database drivers simultaneously. First freeze config and
manifest v1, then build local storage, spool, locking, completion, checksum, and
age plumbing. Add MariaDB first and prove a real disposable restore before S3,
MongoDB, and Redis are added.

