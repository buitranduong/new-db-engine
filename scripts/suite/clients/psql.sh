#!/bin/bash
# psql smoke: connect, SELECT 1, a prepared statement, COPY round trip.
# Needs psql on PATH, or set PSQL="docker run --rm -i --network host postgres:16 psql".
# Uses PGHOST PGPORT PGUSER PGPASSWORD PGDATABASE.
set -eu
PSQL=${PSQL:-psql}
$PSQL -X -v ON_ERROR_STOP=1 -q <<'SQL'
SELECT 1;
DROP TABLE IF EXISTS suite_smoke;
CREATE TABLE suite_smoke (id int PRIMARY KEY, v text);
PREPARE smoke_ins (int, text) AS INSERT INTO suite_smoke VALUES ($1, $2);
EXECUTE smoke_ins(1, 'prepared');
COPY suite_smoke FROM STDIN;
2	copied
3	copied too
\.
COPY suite_smoke TO STDOUT;
-- fails the script (division by zero, ON_ERROR_STOP) unless exactly 3 rows are present
SELECT 1 / (count(*) = 3)::int AS rows_ok FROM suite_smoke;
DROP TABLE suite_smoke;
SQL
echo "OK psql"
