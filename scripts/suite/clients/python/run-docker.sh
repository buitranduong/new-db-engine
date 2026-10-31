#!/bin/bash
# Runs the three Python smokes in python:3.12-slim (host networking). Installs psycopg2-binary,
# psycopg[binary] and sqlalchemy inside the throwaway container only. Env: PGHOST PGPORT PGUSER PGPASSWORD PGDATABASE.
set -eu
cd "$(dirname "$0")"
docker run --rm --network host -v "$PWD":/w:ro -w /w \
  -e PGHOST -e PGPORT -e PGUSER -e PGPASSWORD -e PGDATABASE -e PGSSLMODE \
  python:3.12-slim bash -c "pip install -q --root-user-action=ignore psycopg2-binary 'psycopg[binary]' sqlalchemy 2>&1 | tail -1
    python smoke_psycopg2.py && python smoke_psycopg3.py && python smoke_sqlalchemy.py"
