#!/usr/bin/env bash
set -euo pipefail

# Opt-in, destructive only to its own temporary Docker resources.
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
smoke_root="$(mktemp -d /tmp/savetoa-pg-smoke.XXXXXX)"
smoke_suffix="${smoke_root##*.}"
network="savetoa-pg-smoke-${smoke_suffix}"
primary="${network}-primary"
standby="${network}-standby"
base_restore="${network}-base-restore"
dump_restore="${network}-dump-restore"
standby_volume="${network}-standby-data"
restore_volume="${network}-restore-data"

cleanup() {
  local container volume attempt
  for container in "$dump_restore" "$base_restore" "$standby" "$primary"; do
    docker stop "$container" >/dev/null 2>&1 || true
  done
  for volume in "$restore_volume" "$standby_volume"; do
    docker volume inspect "$volume" >/dev/null 2>&1 || continue
    for ((attempt = 0; attempt < 5; attempt++)); do
      docker volume rm "$volume" >/dev/null 2>&1 && break
      sleep 1
    done
    if docker volume inspect "$volume" >/dev/null 2>&1; then
      printf 'Temporary Docker volume still needs removal: %s\n' "$volume" >&2
    fi
  done
  if ! docker network rm "$network" >/dev/null 2>&1; then
    if docker network inspect "$network" >/dev/null 2>&1; then
      printf 'Temporary Docker network still needs removal: %s\n' "$network" >&2
    fi
  fi
  if [[ "$smoke_root" == /tmp/savetoa-pg-smoke.* ]]; then
    rm -r -- "$smoke_root"
  fi
}
trap cleanup EXIT

wait_for_recovery_state() {
  local container="$1" expected="$2" result
  for ((attempt = 0; attempt < 60; attempt++)); do
    result="$(docker exec "$container" psql --host=127.0.0.1 --username=postgres --dbname=postgres --no-psqlrc --tuples-only --no-align --command='SELECT pg_is_in_recovery()' 2>/dev/null || true)"
    if [[ "$result" == "$expected" ]]; then return 0; fi
    sleep 1
  done
  printf 'PostgreSQL container %s did not reach recovery state %s\n' "$container" "$expected" >&2
  return 1
}

run_savetoa() {
  local source_container="$1"
  shift
  docker run --rm \
    --network "container:${source_container}" \
    --mount "type=bind,src=${smoke_root},dst=/smoke" \
    --entrypoint /bin/sh postgres:18 -ec \
      'mkdir -p /var/lib/savetoa/work /var/lib/savetoa/spool /run/savetoa; exec /smoke/savetoa --config /smoke/config.yml "$@"' \
      sh "$@"
}

extract_backup_id() {
  local output="$1" id
  id="${output#*backup_id=}"
  id="${id%% *}"
  if [[ "$id" == "$output" || -z "$id" || "$id" == *$'\n'* ]]; then
    printf 'Could not read backup ID from SaveToA output\n' >&2
    return 1
  fi
  printf '%s\n' "$id"
}

docker image inspect postgres:18 >/dev/null 2>&1 || docker pull postgres:18 >/dev/null
docker_arch="$(docker image inspect postgres:18 --format '{{.Architecture}}')"
case "$docker_arch" in amd64|arm64) ;; *) printf 'Unsupported Docker architecture: %s\n' "$docker_arch" >&2; exit 1 ;; esac
(
  cd "$repo_root"
  GOOS=linux GOARCH="$docker_arch" CGO_ENABLED=0 go build -trimpath -o "$smoke_root/savetoa" ./cmd/savetoa
)
mkdir "$smoke_root/destination"
printf '127.0.0.1:5432:*:postgres:smoke-only-unused\n' > "$smoke_root/pgpass"
chmod 0600 "$smoke_root/pgpass"
cat > "$smoke_root/config.yml" <<'YAML'
config_version: 1
environment: smoke
targets:
  cluster:
    driver: postgresql-base
    credentials:
      file: /smoke/pgpass
    source:
      host: 127.0.0.1
      port: 5432
      username: postgres
      require_standby: true
    destinations:
      local:
        driver: local
        path: /smoke/destination
  database:
    driver: postgresql-dump
    credentials:
      file: /smoke/pgpass
    source:
      host: 127.0.0.1
      port: 5432
      username: postgres
      database: appdb
    destinations:
      local:
        driver: local
        path: /smoke/destination
groups: {}
YAML

docker network create "$network" >/dev/null
docker run --rm --detach --name "$primary" --network "$network" \
  --env POSTGRES_HOST_AUTH_METHOD=trust postgres:18 >/dev/null
wait_for_recovery_state "$primary" f
docker exec "$primary" psql --host=127.0.0.1 --username=postgres --dbname=postgres \
  --command='CREATE DATABASE appdb' >/dev/null
docker exec "$primary" psql --host=127.0.0.1 --username=postgres --dbname=appdb \
  --command="CREATE TABLE smoke_marker (id integer PRIMARY KEY, value text NOT NULL); INSERT INTO smoke_marker VALUES (1, 'physical-and-logical-smoke');" >/dev/null
docker exec --user postgres "$primary" /bin/sh -ec \
  'printf "host replication postgres all trust\n" >> "$PGDATA/pg_hba.conf"; pg_ctl -D "$PGDATA" reload' >/dev/null

docker volume create "$standby_volume" >/dev/null
docker run --rm --user postgres --network "$network" \
  --mount "type=volume,src=${standby_volume},dst=/var/lib/postgresql" \
  postgres:18 pg_basebackup --host="$primary" --username=postgres \
    --pgdata=/var/lib/postgresql/18/docker --format=plain --wal-method=stream \
    --checkpoint=fast --write-recovery-conf --no-password
docker run --rm --detach --name "$standby" --network "$network" \
  --mount "type=volume,src=${standby_volume},dst=/var/lib/postgresql" \
  postgres:18 >/dev/null
wait_for_recovery_state "$standby" t
standby_row="$(docker exec "$standby" psql --host=127.0.0.1 --username=postgres --dbname=appdb --tuples-only --no-align --command='SELECT value FROM smoke_marker WHERE id = 1')"
[[ "$standby_row" == physical-and-logical-smoke ]]

primary_doctor_output="$(run_savetoa "$primary" doctor cluster 2>&1)" && {
  printf 'Physical doctor unexpectedly accepted the primary\n' >&2
  exit 1
}
[[ "$primary_doctor_output" == *'not a standby'* ]]
run_savetoa "$standby" doctor cluster
run_savetoa "$primary" doctor database
base_output="$(run_savetoa "$standby" run cluster)"
dump_output="$(run_savetoa "$primary" run database)"
printf '%s\n%s\n' "$base_output" "$dump_output"
base_id="$(extract_backup_id "$base_output")"
dump_id="$(extract_backup_id "$dump_output")"
run_savetoa "$standby" restore --source-root /smoke/destination --target-dir /smoke/base-materialized "$base_id"
run_savetoa "$primary" restore --source-root /smoke/destination --target-dir /smoke/dump-materialized "$dump_id"

docker volume create "$restore_volume" >/dev/null
docker run --rm --user postgres \
  --mount "type=bind,src=${smoke_root}/base-materialized,dst=/backup,readonly" \
  --mount "type=volume,src=${restore_volume},dst=/var/lib/postgresql" \
  --entrypoint /bin/sh postgres:18 -ec \
    'mkdir -p "$PGDATA"; tar -xf /backup/base.tar -C "$PGDATA"; tar -xf /backup/pg_wal.tar -C "$PGDATA/pg_wal"; rm -f "$PGDATA/standby.signal" "$PGDATA/recovery.signal"'
docker run --rm --detach --name "$base_restore" --network none \
  --mount "type=volume,src=${restore_volume},dst=/var/lib/postgresql" \
  postgres:18 -c listen_addresses=127.0.0.1 >/dev/null
wait_for_recovery_state "$base_restore" f
base_row="$(docker exec "$base_restore" psql --host=127.0.0.1 --username=postgres --dbname=appdb --tuples-only --no-align --command='SELECT value FROM smoke_marker WHERE id = 1')"
[[ "$base_row" == physical-and-logical-smoke ]]

docker run --rm --detach --name "$dump_restore" --network none \
  --env POSTGRES_HOST_AUTH_METHOD=trust --env POSTGRES_DB=appdb postgres:18 >/dev/null
wait_for_recovery_state "$dump_restore" f
docker cp "$smoke_root/dump-materialized/database.dump" "$dump_restore:/tmp/database.dump"
docker exec "$dump_restore" pg_restore --host=127.0.0.1 --username=postgres \
  --dbname=appdb --exit-on-error --no-owner /tmp/database.dump
dump_row="$(docker exec "$dump_restore" psql --host=127.0.0.1 --username=postgres --dbname=appdb --tuples-only --no-align --command='SELECT value FROM smoke_marker WHERE id = 1')"
[[ "$dump_row" == physical-and-logical-smoke ]]

printf 'PostgreSQL physical standby and logical dump smoke passed\n'
