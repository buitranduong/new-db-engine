#!/bin/bash
# staskd-up.sh: fresh staskd (SCRAM) from /tmp/jdbcreal-bin on pg 28810 / http 28811, data in $RUN/db
set -euo pipefail
R=${RUN:-$HOME/jdbcreal-run}; . "$R/env.sh"
[ -f "$R/staskd.pid" ] && kill "$(cat "$R/staskd.pid")" 2>/dev/null || true
sleep 1
if [ "${KEEP:-0}" != 1 ]; then case "$R/db" in "$HOME"/jdbcreal-run/db) rm -rf "$R/db";; esac; fi
nohup setsid "${BIN:-/tmp/jdbcreal-bin}/staskd" -dir "$R/db" -addr 127.0.0.1:28811 -pg-addr 127.0.0.1:28810 \
  -pg-password "$CRM_DB_PASSWORD" -pg-auth scram -slow-query 1ns </dev/null >"$R/staskd.log" 2>&1 &
echo $! > "$R/staskd.pid"; sleep 2; tail -3 "$R/staskd.log"
