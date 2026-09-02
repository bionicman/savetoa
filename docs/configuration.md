# Configuration contract

The default configuration path is `/etc/savetoa/config.yml`. The installed
package contains no runnable targets.

```yaml
config_version: 1
targets: {}
groups: {}
```

`environment` is required when at least one target exists and becomes the first
component of every backup-set path. The package's empty, non-runnable default
configuration may omit it.

See `packaging/config.example.yml` for the accepted config v1 shape currently
implemented by the parser.

Configuration is limited to 1 MiB and exactly one YAML document. Unknown and
duplicate fields are errors at every level. Environment, target, group, and
destination names use lowercase ASCII letters, digits, `_`, and `-`; they must
begin with a letter or digit and may contain at most 63 characters.

The first implemented target schema is `mariadb`. It requires an absolute
native MariaDB option-file reference, a local socket, the expected replica
source host, port and user, mandatory GTID/lag and preparation safety gates,
and at least one destination. Other capture driver names fail closed until
their own typed schemas are implemented.

`run` supports MariaDB targets with local and S3 destinations. Captures are
staged below `/var/lib/savetoa/spool`, verified, and then delivered to every
configured destination in destination-name order. A failed delivery leaves the
completed staged set intact and reports its relative path. Retry that exact set
without recapturing or re-encrypting it with:

```console
savetoa deliver TARGET S3-DESTINATION BACKUP-ID
```

`restore` does not consult target configuration. It takes a completed local
backup-store root, backup ID, and new absolute destination explicitly. An
encrypted set additionally requires a mode-`0600` X25519 identity file.

## Target semantics

Each target declares:

- `driver`: capture implementation;
- `credentials`: reference to protected source credentials;
- `source`: endpoint, socket, and topology/health requirements;
- `capture`: driver-specific capture options;
- `compression`: optional compression transform;
- `encryption`: optional client-side encryption transform;
- `destinations`: one or more named local or remote sinks;
- `retention`: policy for completed backup sets.

Credentials are referenced by file. Secret values must not be embedded in the
main configuration, target names, destination URLs, or manifests.

MariaDB credentials use a protected native option file (normally mode `0600`):

```ini
[client]
user=savetoa_backup
password=replace-through-secret-management
```

The path is passed as MariaDB's first `--defaults-extra-file` option. SaveToA
never parses, prints, or places the password itself in process arguments.

MariaDB capture runs `mariadb-backup --backup` with replica metadata and safe
replica behavior, explicitly attempts to resume the replica SQL thread even
after cancellation, prepares the captured directory, and refuses to package a
set without recognized prepared-checkpoint and GTID metadata.

Config v1 currently accepts `zstd` compression and `age` recipient-file
encryption. Local destinations require a clean absolute path other than `/`.
S3 destinations require a separate credential-file reference, an HTTPS origin
without URL credentials or a path, an explicit lowercase signing region, a
DNS-compatible bucket, and an optional clean relative object-key prefix. The
regular, non-symlink credentials file must have mode `0600` and this strict
shape:

```yaml
access_key_id: replace-through-secret-management
secret_access_key: replace-through-secret-management
# session_token: optional-temporary-session-token
```

Delivery uses path-style requests and requests conditional object creation. It
holds the target lock, reads before every write, and publishes the payload,
exact manifest, and completion marker in that order. Every successful write is
read back. A retry accepts an existing object only after its size and SHA-256
match, so a discovered conflict is never overwritten. Endpoints should honor
`If-None-Match: *` to extend that guarantee to concurrent writers outside this
SaveToA host. This increment supports payloads through S3's 5-GiB single-PUT
limit; multipart delivery remains future work.

The `age` recipients file is public-key material, not an identity file. It uses
one X25519 `age1...` recipient per line; blank lines and `#` comments are
allowed. Private identities, SSH recipients, and plugin recipients fail
closed.

Retention is optional. When present, its daily, weekly, and monthly counts
must be non-negative and at least one completed backup must be retained.

## Groups

Groups provide `run-group` ergonomics:

```yaml
groups:
  production-databases:
    targets:
      - production-mariadb
      - production-mongodb
      - production-redis
```

Failure behavior, concurrency, and ordering must be explicit before groups are
implemented. A group is not a distributed transaction.

## Scheduling

Scheduling is intentionally outside the YAML contract. A Debian installation
provides `savetoa@.service` and `savetoa@.timer`; configuration management
creates and enables instances with the desired `OnCalendar` override.
