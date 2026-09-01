# Security model

## Principles

- Backups are untrusted input during list, verify, and restore.
- Source, encryption, and destination credentials are independent.
- A production source should not receive backup-repository credentials.
- The ordinary backup identity should not automatically have destructive prune
  permissions on remote storage.
- Secrets never enter argv, logs, errors, manifests, or telemetry.
- Restore never targets a live service by default.

## Encryption

Encryption is optional in the product contract and explicit in each target.
The preferred first implementation is recipient-based `age` encryption using
dedicated X25519 recipients. A backup runner then needs only public recipients;
decryption identities can remain offline and be separately escrowed.

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

## Retention

Retention operates only on valid completed sets. Backup upload credentials and
repository-maintenance credentials should be separable. S3 versioning and
Object Lock may provide an additional immutability layer when the provider
supports them.
