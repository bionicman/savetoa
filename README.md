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
age encryption, durable staging, and delivery to every configured local or S3 destination. MariaDB `doctor`
performs the same read-only replica, GTID, lag, and tool-version gates without
capturing data. `restore` verifies a completed local set and safely materializes
its tar, zstd, and age layers into a new explicit directory without activating
a service. Failed S3 delivery can be retried from the durable spool by backup
ID without capturing or encrypting again. `fetch` verifies a completed S3 set
and atomically imports it into the durable spool for local restore. Other
capture drivers and retention still fail explicitly.

## Intended interface

```console
savetoa run production-mariadb
savetoa deliver production-mariadb offsite <backup-id>
savetoa fetch --spool-root /srv/recovery-spool \
  production-mariadb offsite <backup-id>
savetoa verify <backup-id>
savetoa restore --source-root /var/backups/savetoa \
  --target-dir /srv/restore --identity-file /run/restore.age <backup-id>
savetoa prune production-mariadb
savetoa doctor production-mariadb
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
an unprivileged `savetoa` system account and persistent state below
`/var/lib/savetoa`.

Template units are deliberately not enabled by package installation. A
configuration-management consumer supplies targets, credentials, permissions,
timer schedules, and narrowly scoped database filesystem access.

## Documentation

- [Architecture](docs/architecture.md)
- [Design rationale](docs/design-rationale.md)
- [Configuration contract](docs/configuration.md)
- [Manifest format v1](docs/manifest-v1.md)
- [Security model](docs/security-model.md)
- [Development roadmap](docs/roadmap.md)

## License

SaveToA is released under the [MIT License](LICENSE).
