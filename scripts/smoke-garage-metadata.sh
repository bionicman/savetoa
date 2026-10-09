#!/usr/bin/env bash
set -euo pipefail

# Opt-in Garage snapshot contract probe; touches only private temporary resources.
image=dxflrs/garage:v2.3.0
image_digest=sha256:866bd13ed2038ba7e7190e840482bc27234c4afaf77be8cfa439ae088c1e4690
smoke_root="$(mktemp -d /tmp/savetoa-garage-smoke.XXXXXX)"
smoke_suffix="${smoke_root##*.}"
source_container="savetoa-garage-smoke-${smoke_suffix}-source"
restore_container="savetoa-garage-smoke-${smoke_suffix}-restore"

cleanup() {
  docker stop "$restore_container" "$source_container" >/dev/null 2>&1 || true
  if [[ "$smoke_root" == /tmp/savetoa-garage-smoke.* ]]; then
    rm -r -- "$smoke_root"
  fi
}
trap cleanup EXIT

if ! docker image inspect "$image" >/dev/null 2>&1; then
  printf 'Garage image %s is not available locally\n' "$image" >&2
  exit 1
fi
actual_digest="$(docker image inspect "$image" --format '{{.Id}}')"
if [[ "$actual_digest" != "$image_digest" ]]; then
  printf 'Unexpected Garage image digest: %s\n' "$actual_digest" >&2
  exit 1
fi

mkdir -p "$smoke_root/source/meta" "$smoke_root/source/data" \
  "$smoke_root/restore/meta" "$smoke_root/restore/data"
openssl rand -hex 32 > "$smoke_root/rpc-secret"
chmod 0600 "$smoke_root/rpc-secret"
cat > "$smoke_root/garage.toml" <<'TOML'
replication_factor = 1
metadata_dir = "/smoke/instance/meta"
data_dir = "/smoke/instance/data"
db_engine = "lmdb"
rpc_secret_file = "/smoke/rpc-secret"
rpc_bind_addr = "127.0.0.1:3901"

[s3_api]
api_bind_addr = "127.0.0.1:3900"
s3_region = "garage"
TOML
chmod 0600 "$smoke_root/garage.toml"

docker run --rm --detach --name "$source_container" --network none \
  --mount "type=bind,src=${smoke_root},dst=/smoke" \
  --mount "type=bind,src=${smoke_root}/source,dst=/smoke/instance" \
  "$image" /garage -c /smoke/garage.toml server >/dev/null

source_ready=false
for ((attempt = 0; attempt < 30; attempt++)); do
  if docker exec "$source_container" /garage -c /smoke/garage.toml status >/dev/null 2>&1; then
    source_ready=true
    break
  fi
  sleep 1
done
if [[ "$source_ready" != true ]]; then
  printf 'Garage source did not become ready\n' >&2
  exit 1
fi
node_id="$(docker exec "$source_container" /garage -c /smoke/garage.toml node id 2>/dev/null)"
node_id="${node_id%%@*}"
docker exec "$source_container" /garage -c /smoke/garage.toml layout assign \
  -c 1G -z smoke "$node_id" >/dev/null 2>&1
docker exec "$source_container" /garage -c /smoke/garage.toml layout apply \
  --version 1 >/dev/null 2>&1
docker exec "$source_container" /garage -c /smoke/garage.toml bucket create \
  snapshot-probe >/dev/null 2>&1
if ! docker exec "$source_container" /garage -c /smoke/garage.toml bucket list 2>/dev/null | grep -q snapshot-probe; then
  printf 'Garage source did not list the test bucket\n' >&2
  exit 1
fi

snapshot_ready=false
for ((attempt = 0; attempt < 30; attempt++)); do
  if docker exec "$source_container" /garage -c /smoke/garage.toml meta snapshot >/dev/null 2>&1; then
    snapshot_ready=true
    break
  fi
  sleep 1
done
if [[ "$snapshot_ready" != true ]]; then
  printf 'Garage did not produce a metadata snapshot\n' >&2
  exit 1
fi

snapshot_count=0
snapshot_file=''
while IFS= read -r -d '' candidate; do
  snapshot_file="$candidate"
  ((snapshot_count += 1))
done < <(find "$smoke_root/source/meta/snapshots" -mindepth 2 -maxdepth 2 \
  -name db.lmdb -type f -print0)
if [[ "$snapshot_count" -ne 1 || ! -s "$snapshot_file" ]]; then
  printf 'Expected exactly one nonempty Garage LMDB snapshot\n' >&2
  exit 1
fi
mkdir "$smoke_root/restore/meta/db.lmdb"
cp "$snapshot_file" "$smoke_root/restore/meta/db.lmdb/data.mdb"
cp "$smoke_root/source/meta/node_key" "$smoke_root/restore/meta/node_key"
cp "$smoke_root/source/meta/node_key.pub" "$smoke_root/restore/meta/node_key.pub"
cp "$smoke_root/source/meta/cluster_layout" "$smoke_root/restore/meta/cluster_layout"
docker run --rm --detach --name "$restore_container" --network none \
  --mount "type=bind,src=${smoke_root},dst=/smoke" \
  --mount "type=bind,src=${smoke_root}/restore,dst=/smoke/instance" \
  "$image" /garage -c /smoke/garage.toml server >/dev/null

restore_ready=false
for ((attempt = 0; attempt < 30; attempt++)); do
  if docker exec "$restore_container" /garage -c /smoke/garage.toml status >/dev/null 2>&1; then
    restore_ready=true
    break
  fi
  sleep 1
done
if [[ "$restore_ready" != true ]]; then
  printf 'Garage restore did not become ready\n' >&2
  exit 1
fi
restore_buckets="$(docker exec "$restore_container" /garage -c /smoke/garage.toml bucket list 2>/dev/null || true)"
if [[ "$restore_buckets" != *snapshot-probe* ]]; then
  printf 'Garage snapshot did not retain the test bucket\n' >&2
  exit 1
fi
printf 'Garage v2.3.0 LMDB snapshot replay passed in isolated Docker containers\n'
