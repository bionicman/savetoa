# SaveToA

SaveToA is a job-driven backup orchestrator for databases, filesystems, and
object storage. A named target selects a capture driver, source credentials,
capture policy, transformations, destinations, and retention policy.

The name remembers the old instruction to save a file to drive `A:`. The tool
does not emulate floppy media and is not tied to a particular application.

## Status

SaveToA is an early implementation. Strict config and manifest v1 contracts,
target locking, streaming X25519 `age` encryption, durable spool, and verified
local delivery exist; capture drivers and end-to-end backup commands are not
implemented yet. Commands that could imply data was protected fail explicitly
until their implementation is complete.

## Intended interface

```console
savetoa run production-mariadb
savetoa verify <backup-id>
savetoa restore <backup-id> --target-dir /srv/restore
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

Ubuntu 26.04 is the default test environment. To run the same checks in the
project container:

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
timer schedules, and any database-specific supplementary groups.

## Documentation

- [Architecture](docs/architecture.md)
- [Design rationale](docs/design-rationale.md)
- [Configuration contract](docs/configuration.md)
- [Manifest format v1](docs/manifest-v1.md)
- [Security model](docs/security-model.md)
- [Development roadmap](docs/roadmap.md)

## License

SaveToA is released under the [MIT License](LICENSE).
