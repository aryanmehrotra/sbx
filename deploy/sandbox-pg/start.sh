#!/bin/sh
# Postgres in the background, sbx in the foreground.
#
# sbx is the foreground process on purpose: it is what the platform's health check and its
# scale-to-zero wake talk to, so it is the one whose death should take the container with it.
# Postgres listening only on loopback is not a hardening measure, it is the arrangement - the
# only route in is the tunnel, which is authenticated.
set -eu

if [ -z "${POSTGRES_PASSWORD:-}" ]; then
  echo "POSTGRES_PASSWORD is not set. It is not baked into the image on purpose:" >&2
  echo "pass it at deploy time, alongside SBX_CONNECT_TOKEN." >&2
  exit 1
fi

docker_entrypoint() { exec docker-entrypoint.sh postgres -c listen_addresses=127.0.0.1; }

docker_entrypoint &

# Wait for it to accept before serving the tunnel, so the first connection through does not
# meet a database that has not opened its socket yet. The platform's own wake already makes
# somebody wait; this keeps that wait in one place.
for _ in $(seq 1 60); do
  pg_isready -h 127.0.0.1 -p 5432 >/dev/null 2>&1 && break
  sleep 1
done

exec sbx serve --connect-addr=":${PORT}" --behind-proxy --front="postgres=5432"
