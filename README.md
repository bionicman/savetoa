# SaveToA

SaveToA is a job-driven backup orchestrator for databases, filesystems, and
object storage. A named target selects a capture driver, source credentials,
capture policy, transformations, destinations, and retention policy.

The name remembers the old instruction to save a file to drive `A:`. The tool
does not emulate floppy media and is not tied to a particular application.

## Status

SaveToA is an early implementation. Strict config and manifest v1 contracts,
target locking, streaming X25519 `age` encryption, durable spool, and verified
local storage and idempotent S3 delivery and recovery exist. MariaDB `run` performs replica health gates, physical
capture, replica-thread recovery, preparation, tar/zstd packaging, optional
age encryption, durable staging, and delivery to every configured local or S3 destination.
The separate `mariadb-dump` driver captures one explicitly named database from
a primary or replica with a consistent fixed-argument logical dump; its Docker
smoke test restores tables, a view, a trigger, a routine, and an event.
MongoDB targets
perform equivalent hidden/non-voting SECONDARY gates and capture a full
`mongodump --archive --oplog`. Redis targets require a read-only,
priority-zero replica and package a newly completed `BGSAVE SCHEDULE` RDB.
SQLite targets use the native online backup API through a fixed `sqlite3` CLI
invocation and validate both the live source and standalone snapshot.
PostgreSQL targets separately support whole-cluster standby base backups and
single-database custom-format dumps, each with native archive checks.
`./scripts/smoke-postgresql.sh` exercises both formats and disposable restores
using temporary Docker PostgreSQL 18 instances.
`./scripts/smoke-mariadb-dump.sh` exercises logical MariaDB capture,
materialization, and disposable restore using a temporary Docker MariaDB.
Tar targets archive explicit filesystem paths with fixed GNU tar arguments;
compression and encryption remain common transforms rather than tar options.
`doctor` performs the corresponding topology, lag, persistence, and
tool-version gates without capturing data. `restore` verifies a completed local set and safely materializes
its tar, zstd, and age layers into a new explicit directory without activating
a service. Failed S3 delivery can be retried from the durable spool by backup
ID without capturing or encrypting again. `fetch` verifies a completed S3 set
and atomically imports it into the durable spool for local restore. `prune`
applies UTC daily, ISO-weekly, and monthly retention independently to the
spool and every destination, using separate remote maintenance credentials.
`list` inventories completed sets across the spool and configured destinations;
`status` additionally returns a monitoring-friendly exit code for missing,
incomplete, stale, or clock-skewed latest sets. Both support deterministic JSON.
Optional root-managed lifecycle hooks receive non-secret JSON events after
`run`, delivery, fetch, prune, status, and doctor outcomes.
`restore-mongodb`
replays into a temporary loopback-only `mongod` with a new dbpath and never
accepts a service endpoint. `restore-redis` loads the RDB into a temporary
loopback-only `redis-server` with persistence disabled.

## Intended interface

```console
savetoa run production-mariadb
savetoa deliver production-mariadb offsite <backup-id>
savetoa fetch --spool-root /srv/recovery-spool \
  production-mariadb offsite <backup-id>
savetoa verify <backup-id>
savetoa restore --source-root /var/backups/savetoa \
  --target-dir /srv/restore --identity-file /run/restore.age <backup-id>
savetoa restore-mongodb --source-root /var/backups/savetoa \
  --target-dir /srv/mongodb-restore-check \
  --identity-file /run/restore.age <backup-id>
savetoa restore-redis --source-root /var/backups/savetoa \
  --target-dir /srv/redis-restore-check \
  --identity-file /run/restore.age <backup-id>
savetoa prune production-mariadb
savetoa list --format json production-mariadb
savetoa status --format json --max-age 36h production-mariadb
savetoa doctor production-mariadb
savetoa run production-files
```

`production-mariadb` is a target, not a backup type. Its configuration chooses
the `mariadb` driver and all settings needed to run that particular job.

## Build

```console
make build
make test
```

The resulting development binary is `build/savetoa`.

Ubuntu 26.04 with Go 1.27.1 is the default test environment. The container
downloads the pinned Go release from `go.dev` and verifies its SHA-256 before
installation. To run the same checks in the project container:

```console
docker build --tag savetoa-test .
docker run --rm savetoa-test
```

The image runs `make check` by default.

## Debian package

The repository contains conventional debhelper packaging:

```console
dpkg-buildpackage -us -uc -b
lintian ../savetoa_*.changes
```

The package installs the binary as `/usr/bin/savetoa`, reads configuration
from `/etc/savetoa/config.yml`, and provides systemd template units. It creates
unprivileged `savetoa` and `savetoa-maintenance` system accounts and persistent
state below `/var/lib/savetoa`. A successful backup unit triggers its matching
prune unit; a target without retention configured is a successful no-op.

Template units are deliberately not enabled by package installation. A
configuration-management consumer supplies targets, credentials, permissions,
timer schedules, alert routing, and narrowly scoped database filesystem access.
The optional status timer reads a per-target freshness threshold from a
non-secret environment file and leaves failed checks visible to systemd.

## Documentation

- [Architecture](docs/architecture.md)
- [Design rationale](docs/design-rationale.md)
- [Configuration contract](docs/configuration.md)
- [Manifest format v1](docs/manifest-v1.md)
- [Status report v1](docs/status-v1.md)
- [Lifecycle hooks v1](docs/hooks-v1.md)
- [Security model](docs/security-model.md)
- [Development roadmap](docs/roadmap.md)

## License

SaveToA is released under the [MIT License](LICENSE).
