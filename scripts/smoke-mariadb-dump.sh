#!/usr/bin/env bash
set -euo pipefail

# Opt-in; destructive only to its own temporary Docker containers and directory.
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
smoke_root="$(mktemp -d /tmp/savetoa-mariadb-smoke.XXXXXX)"
smoke_suffix="${smoke_root##*.}"
source_container="savetoa-mariadb-smoke-${smoke_suffix}-source"
restore_container="savetoa-mariadb-smoke-${smoke_suffix}-restore"
image="mariadb:11.8"

cleanup() {
  docker stop "$restore_container" "$source_container" >/dev/null 2>&1 || true
  if [[ "$smoke_root" == /tmp/savetoa-mariadb-smoke.* ]]; then
    rm -r -- "$smoke_root"
  fi
}
trap cleanup EXIT

wait_for_mariadb() {
  local container="$1"
  for ((attempt = 0; attempt < 60; attempt++)); do
    # The image uses a temporary socket-only server during initialization.
    # Waiting on TCP avoids racing that server's intentional shutdown.
    if docker exec "$container" mariadb-admin --protocol=TCP --host=127.0.0.1 --user=root ping >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  printf 'MariaDB container %s did not become ready\n' "$container" >&2
  return 1
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

docker image inspect "$image" >/dev/null 2>&1 || docker pull "$image" >/dev/null
docker_arch="$(docker image inspect "$image" --format '{{.Architecture}}')"
case "$docker_arch" in amd64|arm64) ;; *) printf 'Unsupported Docker architecture: %s\n' "$docker_arch" >&2; exit 1 ;; esac
(
  cd "$repo_root"
  GOOS=linux GOARCH="$docker_arch" CGO_ENABLED=0 go build -trimpath -o "$smoke_root/savetoa" ./cmd/savetoa
)
mkdir "$smoke_root/destination"
printf '[client]\nuser=root\n' > "$smoke_root/mariadb.cnf"
chmod 0600 "$smoke_root/mariadb.cnf"
cat > "$smoke_root/config.yml" <<'YAML'
config_version: 1
environment: smoke
targets:
  application:
    driver: mariadb-dump
    credentials:
      file: /smoke/mariadb.cnf
    source:
      socket: /run/mysqld/mysqld.sock
      database: appdb
    compression:
      driver: zstd
      level: 3
    destinations:
      local:
        driver: local
        path: /smoke/destination
groups: {}
YAML

docker run --rm --detach --name "$source_container" \
  --mount "type=bind,src=${smoke_root},dst=/smoke" \
  --env MARIADB_ALLOW_EMPTY_ROOT_PASSWORD=1 "$image" >/dev/null
wait_for_mariadb "$source_container"
docker exec -i "$source_container" mariadb --user=root <<'SQL'
CREATE DATABASE appdb CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
USE appdb;
CREATE TABLE smoke_marker (id INT PRIMARY KEY, value VARCHAR(100) NOT NULL);
INSERT INTO smoke_marker VALUES (1, 'logical-dump-smoke');
CREATE VIEW smoke_view AS SELECT value FROM smoke_marker;
CREATE TABLE smoke_audit (value VARCHAR(100) NOT NULL);
CREATE TRIGGER smoke_trigger AFTER INSERT ON smoke_marker FOR EACH ROW INSERT INTO smoke_audit VALUES (NEW.value);
CREATE PROCEDURE smoke_procedure() SELECT COUNT(*) AS marker_count FROM smoke_marker;
CREATE EVENT smoke_event ON SCHEDULE EVERY 1 DAY DO INSERT INTO smoke_audit VALUES ('event');
SQL
docker exec "$source_container" mkdir -p /var/lib/savetoa/work /var/lib/savetoa/spool /run/savetoa
docker exec "$source_container" /smoke/savetoa --config /smoke/config.yml doctor application
run_output="$(docker exec "$source_container" /smoke/savetoa --config /smoke/config.yml run application)"
printf '%s\n' "$run_output"
backup_id="$(extract_backup_id "$run_output")"
docker exec "$source_container" /smoke/savetoa restore \
  --source-root /smoke/destination --target-dir /smoke/materialized "$backup_id"

docker run --rm --detach --name "$restore_container" \
  --env MARIADB_ALLOW_EMPTY_ROOT_PASSWORD=1 "$image" >/dev/null
wait_for_mariadb "$restore_container"
docker exec -i "$restore_container" mariadb --user=root < "$smoke_root/materialized/database.sql"
marker="$(docker exec "$restore_container" mariadb --user=root --batch --skip-column-names appdb -e 'SELECT value FROM smoke_view WHERE value = "logical-dump-smoke"')"
[[ "$marker" == logical-dump-smoke ]]
docker exec "$restore_container" mariadb --user=root appdb -e "INSERT INTO smoke_marker VALUES (2, 'trigger-restored')"
triggered="$(docker exec "$restore_container" mariadb --user=root --batch --skip-column-names appdb -e 'SELECT COUNT(*) FROM smoke_audit WHERE value = "trigger-restored"')"
[[ "$triggered" == 1 ]]
procedure="$(docker exec "$restore_container" mariadb --user=root --batch --skip-column-names appdb -e 'CALL smoke_procedure()' | tail -1)"
[[ "$procedure" == 2 ]]
event_count="$(docker exec "$restore_container" mariadb --user=root --batch --skip-column-names information_schema -e 'SELECT COUNT(*) FROM EVENTS WHERE EVENT_SCHEMA = "appdb" AND EVENT_NAME = "smoke_event"')"
[[ "$event_count" == 1 ]]

printf 'MariaDB logical dump capture and disposable restore smoke passed\n'
