#!/bin/sh
# Migration sidecar entrypoint (M10.3 + iter-1).
#
# Runs `goose -dir /migrations postgres "${DSN}" <args>`. The DSN is
# assembled INSIDE this script from individual components
# (POSTGRES_USER + POSTGRES_PASSWORD + POSTGRES_HOST + POSTGRES_PORT
# + POSTGRES_DB) with URL-encoding applied to the password so an
# operator can use any byte sequence (including the URI-reserved
# `@:/?#` characters) without breaking the goose CLI. The password
# itself may be supplied via POSTGRES_PASSWORD_FILE (a docker-secrets
# mount) so the compose stack never embeds the cleartext in env vars
# visible to `docker inspect` — addresses iter-1 codex-P1 + critic-#1.
#
# Backwards-compat: if DATABASE_URL is set, it overrides the
# component-based assembly. M10.3 iter-0 callers and any operator
# already using the pre-iter-1 shape keep working.
#
# Default CMD is `up`; operator can override with
# `docker compose run --rm migrate down 1`. A trailing summary line
# names the verb that ran so `compose logs migrate` is grep-friendly
# under M10.4 smoke triage (iter-1 #14).
#
# POSIX sh deliberately — alpine:3.20 ships ash. Shellcheck's bash
# style suggestions (SC2292 prefer [[ ]]) are bashisms that ash does
# not support; the wrapper stays portable.
# shellcheck shell=sh
set -eu

# url_encode prints stdin to stdout with every byte outside the
# unreserved RFC3986 set (`[A-Za-z0-9-._~]`) percent-encoded. awk +
# printf only — no python / perl dependency in the runtime image.
url_encode() {
    awk '
        BEGIN {
            for (i = 0; i < 256; i++) {
                hex[sprintf("%c", i)] = sprintf("%%%02X", i)
            }
        }
        {
            n = length($0)
            for (i = 1; i <= n; i++) {
                c = substr($0, i, 1)
                if (c ~ /[A-Za-z0-9._~-]/) {
                    printf "%s", c
                } else {
                    printf "%s", hex[c]
                }
            }
        }
    '
}

read_secret() {
    # Mirrors keep.config.envValue: if `<NAME>_FILE` is set, read from
    # that file (trimming the trailing newline). Otherwise return the
    # plain env value, possibly empty.
    name="$1"
    eval "file_path=\${${name}_FILE-}"
    if [ -n "${file_path}" ]; then
        if [ ! -r "${file_path}" ]; then
            echo "migrate: ${name}_FILE (${file_path}) is not readable" >&2
            exit 2
        fi
        tr -d '\n' < "${file_path}"
        return
    fi
    eval "printf '%s' \"\${${name}-}\""
}

if [ -n "${DATABASE_URL:-}" ]; then
    dsn="${DATABASE_URL}"
else
    pg_user=$(read_secret POSTGRES_USER)
    pg_password=$(read_secret POSTGRES_PASSWORD)
    pg_host="${POSTGRES_HOST:-postgres}"
    pg_port="${POSTGRES_PORT:-5432}"
    pg_db="${POSTGRES_DB:-keep}"
    pg_sslmode="${POSTGRES_SSLMODE:-disable}"

    if [ -z "${pg_user}" ]; then
        echo "migrate: POSTGRES_USER (or POSTGRES_USER_FILE) is required" >&2
        exit 2
    fi
    if [ -z "${pg_password}" ]; then
        echo "migrate: POSTGRES_PASSWORD (or POSTGRES_PASSWORD_FILE) is required" >&2
        exit 2
    fi

    encoded_password=$(printf '%s' "${pg_password}" | url_encode)
    encoded_user=$(printf '%s' "${pg_user}" | url_encode)
    dsn="postgres://${encoded_user}:${encoded_password}@${pg_host}:${pg_port}/${pg_db}?sslmode=${pg_sslmode}"
fi

verb="${1:-up}"
goose -dir /migrations postgres "${dsn}" "$@"
echo "migrate: complete (${verb})"
