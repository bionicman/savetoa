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
non-canonical paths, traversal, duplicates, links, and special files; a failed
materialization removes only the destination it created. Restore never starts,
stops, or replaces a service.

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

The Debian package creates an unprivileged `savetoa` account. Package
installation does not grant access to a database datadir, socket, secrets, or
remote storage. A consumer may add narrowly scoped supplementary groups or a
reviewed systemd drop-in for a particular target after validating the native
backup tool's actual permission requirements.

The generic YAML configuration must never offer a `run_as: root` switch.

MongoDB credentials contain only a password in a mode-0600 file. Health checks
use a direct official-driver connection to the configured member, and native
capture receives that same file through `mongodump --config`; neither path
constructs a secret-bearing URI or reports native error text. Full+oplog is
mandatory and filtering options are absent from config v1.

MongoDB replay is confined to a new operator-selected directory and a
SaveToA-launched loopback-only `mongod`. The command has no host, port,
credentials, drop, or live-service activation option. It removes the directory
it created on failure and stops the disposable server before reporting success.

MariaDB capture directories are private and short-lived. Archive traversal
rejects symlinks and special files, and only the resulting tar stream enters
the durable spool. On cancellation or native-tool failure, SaveToA uses a
separate bounded cleanup context to attempt to resume the replica SQL thread.

## Retention

Retention operates only on valid completed sets. Backup upload credentials and
repository-maintenance credentials should be separable. S3 versioning and
Object Lock may provide an additional immutability layer when the provider
supports them.
