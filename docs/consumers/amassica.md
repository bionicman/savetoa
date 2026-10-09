# First consumer: Amassica

This file records the first consumer's constraints so generic implementation
work can be validated against a real topology. These names and policies must
not be compiled into SaveToA.

## Infrastructure repository

The consumer configuration is managed by the sibling private deployment
repository `amassica/amassica-infra`. It uses Ansible inventory, group vars,
host vars, Vault secrets, and systemd units. SaveToA should eventually be
installed there from a pinned Debian package version.

## Backup runner

The production backup runner is:

```text
amassica-backup-production-fra-0.firehub.net
```

It currently has Ubuntu 26.04, 4 vCPU, 6 GiB RAM, and a 100 GB ext4 root disk
with ample space for the initial data volume. It is a backup/replica host, not
an application, ingress, failover, or asset-management host.

Production services communicate with it through a dual-stack WireGuard
overlay. Backup credentials remain on this host; ordinary production servers
must not receive destination credentials.

## Existing local shadow replicas

### MariaDB

- MariaDB 12.3, matching `mariadb-backup` package.
- Read-only GTID replica with relay-log recovery.
- Source channel currently prefers the IPv6 WireGuard overlay.
- The replica has already passed restart recovery and a replicated data probe.
- Current dataset was roughly 155 MB when the backup host was commissioned.

The first intended target name is `production-mariadb`. SaveToA must capture
and prepare a physical backup from this local replica, including replication
coordinates. The Ansible consumer will determine how the unprivileged
`savetoa` account receives the minimum filesystem/socket access required by
the installed MariaDB version.

### MongoDB

- MongoDB 8.3 replica set named `amassica-production`.
- Backup member is hidden, non-voting, priority zero, and healthy SECONDARY.
- The member has passed restart recovery and document replication probes.
- Current dataset was roughly 257 MB at commissioning.

The intended target name is `production-mongodb`. It requires a full archive
with oplog from the local member and must not narrow capture to the application
database even though that database is the main restore interest.

### Redis

- Redis 8.0.5 read-only replica.
- Replica follows the production source through the IPv6 WireGuard overlay.
- Promotion priority is zero and no Sentinel runs on the backup host.
- It has passed restart recovery and SET/GET/DEL replication probes.

The intended target name is `production-redis`. SaveToA must create a fresh RDB
only after the replication and BGSAVE health gates pass.

## Additional future assets

- `/var/lib/acme.sh` and deployed TLS key material must eventually be backed up.
  Domain keys are deliberately stable because future TLSA records may pin them.
- Production Garage currently runs as a standalone store. A future replicated
  Garage is a separate store and migration destination, not an in-place change
  of the current replication factor.
- Garage backup requires live metadata snapshots plus object copies through
  the S3 API. Raw copying of its active data directory is not an adequate sole
  backup.
- A later generic store migration layer must support disk-to-S3, disk-to-disk,
  and S3-to-S3 flows.

## Initial destination policy

The first rollout writes completed artifacts locally on the backup host. A
second S3-compatible destination is required later because the replicas and
local backup files currently share one VM and one root disk.

Client-side age encryption should be enabled by production policy even though
the SaveToA product keeps encryption optional. The backup host should hold
public recipients only. Remote prune credentials should be separable from
ordinary upload credentials.

## Restore expectations

Replica data is not itself a backup. Success requires scheduled materialized
artifacts, retention, monitoring, and recurring restore exercises:

- MariaDB restore into a disposable datadir/service;
- MongoDB restore with oplog replay into a disposable replica-set instance;
- Redis RDB load into a disposable Redis instance;
- documented observed RPO and RTO.

