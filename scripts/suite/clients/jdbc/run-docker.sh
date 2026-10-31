#!/bin/bash
# Runs Smoke.java in eclipse-temurin:17 (host networking, so PGHOST=127.0.0.1 reaches the host).
# Downloads the pgjdbc jar from Maven Central into a temporary directory; removes it afterwards.
# Env: PGHOST PGPORT PGUSER PGPASSWORD PGDATABASE (or JDBC_URL), PGJDBC_VERSION (default 42.7.4).
set -eu
cd "$(dirname "$0")"
V=${PGJDBC_VERSION:-42.7.4}
W=$(mktemp -d)
trap 'rm -rf "$W"' EXIT
cp Smoke.java "$W/"
docker run --rm --network host -v "$W":/w -w /w \
  -e PGHOST -e PGPORT -e PGUSER -e PGPASSWORD -e PGDATABASE -e PGSSLMODE -e JDBC_URL \
  eclipse-temurin:17 bash -c "
    (command -v curl >/dev/null && curl -fsSL -o pg.jar https://repo1.maven.org/maven2/org/postgresql/postgresql/$V/postgresql-$V.jar) ||
    wget -q -O pg.jar https://repo1.maven.org/maven2/org/postgresql/postgresql/$V/postgresql-$V.jar
    javac -cp pg.jar Smoke.java && java -cp pg.jar:. Smoke"
