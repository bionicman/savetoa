# Configuration contract

The default configuration path is `/etc/savetoa/config.yml`. The installed
package contains no runnable targets.

```yaml
config_version: 1
targets: {}
groups: {}
```

See `packaging/config.example.yml` for the proposed full shape. It is a design
fixture, not yet an accepted parser schema.

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
