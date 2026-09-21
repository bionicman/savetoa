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

The implemented target schemas are `mariadb`, `mongodb`, `redis`, `sqlite3`, and `tar`. MariaDB requires
an absolute native option-file reference, a local socket, the expected replica
source host, port and user, mandatory GTID/lag and preparation safety gates,
and at least one destination. Other capture driver names fail closed until
their own typed schemas are implemented.

`run` supports MariaDB, MongoDB, Redis, SQLite, and tar targets with local and S3 destinations. Captures are
staged below `/var/lib/savetoa/spool`, verified, and then delivered to every
configured destination in destination-name order. A failed delivery leaves the
completed staged set intact and reports its relative path. Retry that exact set
without recapturing or re-encrypting it with:

```console
savetoa deliver TARGET S3-DESTINATION BACKUP-ID
```

Recover that exact remotely completed set into the durable spool with:

```console
savetoa fetch [--spool-root ROOT] TARGET S3-SOURCE BACKUP-ID
```

`fetch` derives the immutable object path from the UTC date prefix in a
SaveToA-generated backup ID. It reads and validates the completion marker and
canonical manifest before requesting the payload, then atomically publishes a
local spool set only after the payload size and SHA-256 match. Repeating a fetch
verifies and reuses an identical local set; it never replaces a conflicting or
invalid published path.
The spool root defaults to `/var/lib/savetoa/spool`; an operator can select a
different existing absolute root for an isolated recovery exercise.

`list [--spool-root ROOT] [--format text|json] TARGET` reads completed sets
from the spool and every configured destination. It reports which repositories
contain each immutable backup ID and ignores incomplete sets. `status` accepts
the same options plus an optional positive `--max-age DURATION`; it exits
nonzero when no completed set exists, the latest set is missing from any
repository, its completion time is in the future, or it exceeds the age limit.
Repository scan errors and conflicting metadata for one backup ID fail closed.
See `docs/status-v1.md` for the stable JSON shape.

Target YAML does not configure executable hooks. An administrator may install
optional root-owned lifecycle executables below `/etc/savetoa/hooks.d`; they
receive the stable non-secret JSON contract documented in `docs/hooks-v1.md`.
SaveToA never evaluates a hook through a shell.

`restore` does not consult target configuration. It takes a completed local
backup-store root, backup ID, and new absolute destination explicitly. An
encrypted set additionally requires a mode-`0600` X25519 identity file.

## Target semantics

Each target declares:

- `driver`: capture implementation;
- `credentials`: reference to protected source credentials when the driver needs them;
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

MongoDB credentials use a protected YAML file containing only the password:

```yaml
password: replace-through-secret-management
```

The regular, non-symlink file must have mode `0600`. The non-secret username,
authentication database, local host and port remain in the target. SaveToA
uses the password in memory for its direct Go-driver health checks and passes
the same file to `mongodump --config`; it never puts the password or a
credential-bearing URI in argv.

MongoDB config requires every replica safety gate: the expected set name,
SECONDARY state, hidden membership, zero votes, priority zero, and bounded
optime lag. Capture is always full and always includes `--oplog`; config v1 has
no database, collection, query, or other filtering fields. The native result is
stored as `dump.archive` inside the standard payload tar before compression or
encryption.

`restore-mongodb` takes the same local source, backup ID, target directory and
optional age identity as `restore`. It additionally preflights matching
MongoDB server major/minor and exact Database Tools versions, then replays the
archive with `--oplogReplay --stopOnError` into a temporary loopback-only
`mongod`. The target directory must not exist. No live MongoDB endpoint can be
supplied to this command.

Redis credentials use the same strict password-only YAML shape as MongoDB and
must be a regular mode-`0600` file. The non-secret ACL username remains in the
target. Native commands receive the password only through `REDISCLI_AUTH`;
SaveToA removes any inherited value first and never includes it in argv or
native error output.

Redis config requires the expected upstream host and port, an up replication
link with no sync in progress, bounded `master_last_io_seconds_ago`,
`slave_read_only=1`, and `slave_priority=0`. The configured absolute
`source.rdb_file` must match the effective Redis `dir` and `dbfilename`.
Capture requires `bgsave: true`, `schedule: true`, and a positive bounded
`max_wait`. It records `LASTSAVE` before requesting `BGSAVE SCHEDULE`,
waits for a newer successful completion, and packages exactly one `dump.rdb`.

`restore-redis` takes the same local source, backup ID, new target directory,
and optional age identity as `restore`. It requires matching Redis
server/CLI versions and loads the RDB in a temporary loopback-only
`redis-server` with AOF and automatic saves disabled. No live Redis endpoint
or credentials can be supplied.

SQLite targets require one canonical absolute database path and accept neither
credentials nor capture options:

```yaml
driver: sqlite3
source:
  path: /var/lib/example/database.sqlite3
compression:
  driver: zstd
  level: 3
```

At runtime the source must be a regular file, not a symlink, and no component
of its configured path may traverse a symlink. SaveToA invokes the fixed
`/usr/bin/sqlite3` executable directly in read-only, no-follow mode. It runs a
fixed `PRAGMA quick_check`, creates a consistent standalone snapshot through
SQLite's online backup command, normalizes the snapshot to rollback-journal
mode, and checks it read-only before the common tar, zstd, age, spool, and
destination pipeline. The output is named
`database.sqlite3` inside the payload. Committed WAL contents are incorporated
by the online backup; `-wal` and `-shm` files are not copied separately.
For a live WAL-mode source, the execution identity must have the filesystem
access SQLite requires to open the database and its existing WAL/SHM state;
those narrowly scoped permissions belong to deployment configuration.

The configured source path is passed only as a native argv element. It never
enters SQL or dot-command text, and configuration cannot select an executable,
SQLite flags, SQL, output filename, or shell fragment. The ordinary `restore`
command safely materializes `database.sqlite3` into a new explicit directory
without replacing or opening a live application database.

Tar targets require 1 to 128 clean, absolute, non-root paths and do not accept
credentials or capture options:

```yaml
driver: tar
source:
  paths:
    - /etc/example-state
    - /var/lib/example-state
compression:
  driver: zstd
  level: 3
```

Configured paths must not be duplicated or overlap. At runtime each configured
root must be a real directory or regular file rather than a symlink. Directory
trees may contain regular files, directories, and relative symlinks whose
resolved targets remain inside one of the configured roots; sockets, devices,
FIFOs, and escaping or absolute symlinks fail closed. SaveToA invokes the fixed
`/usr/bin/tar` executable directly with fixed GNU tar arguments and an option
terminator. There is no configurable executable, argument list, shell text, or
tar compression flag.

Archive entries are rooted below `/` without a leading slash, so restoring the
example into `/srv/restore` creates `/srv/restore/etc/example-state` and
`/srv/restore/var/lib/example-state`. The ordinary `restore` command rejects
absolute paths, traversal, unsafe links, special files, duplicate entries, and
an existing destination. It preserves executable bits and modification times;
when invoked as root it also restores numeric ownership. zstd compression and
age encryption remain common transforms outside the tar driver.

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

S3 credentials used by `fetch` require object-read access. No list or delete
permission is required: the object path is deterministic from the configured
environment, target, destination prefix, and generated backup ID.

When a target has retention, every S3 destination additionally requires a
different `maintenance_credentials.file`. Its strict YAML shape is the same as
the upload credentials, but the identity needs list, metadata-read, and object-
delete access. The path must differ from the upload credential path. Backup
units cannot access the maintenance credential directory, and prune units
cannot access source, upload, or recipient files.

The `age` recipients file is public-key material, not an identity file. It uses
one X25519 `age1...` recipient per line; blank lines and `#` comments are
allowed. Private identities, SSH recipients, and plugin recipients fail
closed.

Retention is optional. When present, its daily, weekly, and monthly counts
must be non-negative and at least one completed backup must be retained.
`prune [--spool-root ROOT] TARGET` evaluates the durable spool and every target
destination independently. It keeps the newest completed set in each of the
newest configured UTC-day, ISO-week, and UTC-month buckets; the union of those
sets is retained. Incomplete sets are ignored. A corrupt completed set aborts
planning before any repository is modified.

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
creates and enables instances with the desired `OnCalendar` override. A
successful `savetoa@TARGET.service` triggers `savetoa-prune@TARGET.service`.
Prune has no independent timer and is a successful no-op when the target has no
retention policy.

The package also provides disabled `savetoa-status@.service` and
`savetoa-status@.timer` templates. Configuration management must create
`/etc/savetoa/status.d/TARGET` containing a non-secret
`SAVETOA_MAX_AGE=DURATION` before it enables an instance. The hourly timer is a
default polling cadence, not an alert transport; operators remain responsible
for connecting failed units to their monitoring system. Package installation
does not enable either backup or status schedules.
