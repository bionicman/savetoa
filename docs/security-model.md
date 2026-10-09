# Security model

## Principles

- Backups are untrusted input during list, verify, and restore.
- Source, encryption, and destination credentials are independent.
- A production source should not receive backup-repository credentials.
- The ordinary backup identity should not automatically have destructive prune
  permissions on remote storage.
- Secrets never enter argv, logs, errors, manifests, or telemetry.
- Restore never targets a live service by default.

Restore verifies the completion marker, manifest binding, payload size, and
SHA-256 before creating its destination. The destination must be an explicit,
absolute, non-existing directory. Tar extraction rejects absolute and
non-canonical paths, traversal, duplicates, hard links, escaping symlinks, and
special files. Safe relative symlinks cannot be used as extraction parents; a
failed materialization removes only the destination it created. Restore never
starts, stops, or replaces a service.

S3 recovery treats every remote byte as untrusted. It validates the requested
set path before network access, reads the completion marker before manifest and
payload, requires the manifest's exact canonical encoding, bounds metadata
reads, and limits payload input to one byte beyond the declared size. An
invalid or interrupted fetch cannot publish a completed local set.

## Encryption

Encryption is optional in the product contract and explicit in each target.
The preferred first implementation is recipient-based `age` encryption using
dedicated X25519 recipients. A backup runner then needs only public recipients;
decryption identities can remain offline and be separately escrowed.

The restore command accepts a private X25519 identity only through a regular,
non-symlink file with mode `0600`. It does not accept an identity value in argv
or copy rejected identity material into an error.

Recipient files contain one public `age1...` X25519 recipient per line, with
blank lines and `#` comments allowed. SSH recipients, plugin recipients, and
identity strings are rejected. Manifests record only SHA-256 fingerprints of
the canonical public recipients. Parser errors never include rejected input,
so an accidentally supplied identity is not copied into logs.

Production policy may require client-side encryption for both local and S3
destinations. S3 server-side encryption is an independent defense and is not a
replacement for client-side encryption.

## Execution identity

The Debian package creates unprivileged `savetoa` and `savetoa-maintenance`
accounts. Package
installation does not grant access to a database datadir, socket, secrets, or
remote storage. A consumer may add narrowly scoped supplementary groups or a
reviewed systemd drop-in for a particular target after validating the native
backup tool's actual permission requirements.

The generic YAML configuration must never offer a `run_as: root` switch.

The backup service cannot access `/etc/savetoa/maintenance-credentials.d`.
The maintenance service cannot access source/upload credentials or encryption
recipients. They share group-writable completed-set roots and the target-lock
directory, but database filesystem ACLs remain assigned only to `savetoa`.

Tar targets have no credentials or configurable command arguments. SaveToA
invokes `/usr/bin/tar` directly with a minimal fixed environment and an option
terminator. It rejects symlinked roots, overlapping roots, special files, and
links whose resolved target falls outside all configured roots before capture.

SQLite targets likewise have no credentials or configurable commands. The
source is one canonical absolute regular file whose path may not traverse a
symlink. SaveToA invokes `/usr/bin/sqlite3` directly with fixed read-only,
no-follow, quick-check, and online-backup operations. The destination filename
is fixed and relative to a private work directory, so configured paths never
enter SQLite command text. Native output is not copied into errors. Access to
the database and any live WAL/SHM state must be granted narrowly by deployment
configuration; package installation grants none.

PostgreSQL capture uses fixed native tools and a minimal child environment.
The only credential input is a protected mode-0600 native `pgpass` file named
through `PGPASSFILE`; passwords do not enter process arguments, errors, or
manifests. Physical capture rejects a primary and uses tar format so external
tablespaces cannot cause `pg_basebackup` to write to their original paths.
PostgreSQL native stdout and stderr are discarded on failure. PostgreSQL
restore remains operator-controlled materialization into a new directory;
SaveToA does not connect to or overwrite a running PostgreSQL service.

MongoDB credentials contain only a password in a mode-0600 file. Health checks
use a direct official-driver connection to the configured member, and native
capture receives that same file through `mongodump --config`; neither path
constructs a secret-bearing URI or reports native error text. Full+oplog is
mandatory and filtering options are absent from config v1.

MongoDB replay is confined to a new operator-selected directory and a
SaveToA-launched loopback-only `mongod`. The command has no host, port,
credentials, drop, or live-service activation option. It removes the directory
it created on failure and stops the disposable server before reporting success.

Redis passwords are read from a mode-0600 file and exposed to `redis-cli` only
through a scrubbed `REDISCLI_AUTH` environment. Capture refuses a writable,
promotable, syncing, stale, or wrong-upstream replica. The configured RDB path
must match Redis itself; symlinks, empty files, stale mtimes, and an inode
replacement during copy are rejected.

Redis restore accepts no endpoint and starts only a loopback disposable server
inside a new explicit directory. AOF and automatic saves are disabled, the
process must remain alive through RDB load and PING, and clean shutdown is
required before success.

MariaDB capture directories are private and short-lived. Archive traversal
rejects symlinks and special files, and only the resulting tar stream enters
the durable spool. On cancellation or native-tool failure, SaveToA uses a
separate bounded cleanup context to attempt to resume the replica SQL thread.

MariaDB logical capture accepts only a local socket, one validated database
name, and a non-empty regular mode-`0600` native option file. The option file is
the first native argument and its contents are never parsed or logged. Dump
flags are fixed: configuration cannot supply SQL, executables, arguments, a
remote host, or multiple databases. Native stderr is discarded. A dump is not
published unless `mariadb-dump` exits successfully and its regular mode-`0600`
output ends with the native completion marker. Server accounts and grants are
outside this single-database contract and remain configuration-management state.

## Retention

Retention operates only on sets whose completion marker binds a valid
manifest. It validates all repository plans before deletion and revalidates a
set immediately before modifying it. Marker-first deletion makes any
interrupted cleanup invisible to restore and future retention. S3 upload and
maintenance credential paths must be distinct. S3 versioning and Object Lock
may provide an additional immutability layer when the provider supports them.
