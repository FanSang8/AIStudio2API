#!/usr/bin/env bash
set -euo pipefail

root=/opt/aistudio2api
source_root="$root/source"
commit="${1:?Pass the pushed commit SHA}"
if [[ ! "$commit" =~ ^[0-9a-f]{40}$ ]]; then
  printf '%s\n' 'Expected a full commit SHA.' >&2
  exit 1
fi
cd "$source_root"
git fetch origin main
git merge-base --is-ancestor "$commit" origin/main
if [[ -n "$(git status --porcelain)" ]]; then
  printf '%s\n' 'Source checkout has local changes; deployment stopped.' >&2
  exit 1
fi
git checkout --detach "$commit"
docker build --label "org.opencontainers.image.revision=$commit" \
  -t "aistudio2api-custom:$commit" .

previous="$(docker inspect --format '{{.Image}}' aistudio2api)"
docker tag "$previous" aistudio2api-custom:rollback
docker tag "aistudio2api-custom:$commit" aistudio2api-custom:local
backup="$(mktemp -d "$root/deploy-backup.XXXXXX")"
for file in compose.yml compose.override.yml; do
  if [[ -f "$root/$file" ]]; then cp "$root/$file" "$backup/$file"; fi
done
rollback() {
  trap - ERR
  docker tag "$previous" aistudio2api-custom:local
  for file in compose.yml compose.override.yml; do
    if [[ -f "$backup/$file" ]]; then cp "$backup/$file" "$root/$file"; fi
  done
  (cd "$root" && docker compose up -d --no-deps --force-recreate aistudio2api)
}
trap rollback ERR
cp deploy/server/compose.yml "$root/compose.yml"
cp deploy/server/compose.proxy.yml "$root/compose.proxy.yml"
cp deploy/server/compose.proxy.yml "$root/compose.override.yml"
cp deploy/server/configure-r2.py "$root/configure-r2.py"
cp deploy/server/configure-access.py "$root/configure-access.py"
cp deploy/server/r2.env.example "$root/r2.env.example"
cd "$root"
docker compose config --quiet
docker compose up -d --no-deps --force-recreate aistudio2api
healthy=false
for attempt in {1..40}; do
  if curl --fail --silent --max-time 2 http://127.0.0.1:12048/health >/dev/null; then
    healthy=true
    break
  fi
  sleep 1
done
if [[ "$healthy" != true ]]; then
  printf '%s\n' 'Health check failed; restoring the previous container.' >&2
  false
fi
trap - ERR
printf 'Deployed commit %s\n' "$commit"
