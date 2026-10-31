#!/bin/bash
# run-pgjdbc.sh [single|cluster|pg|all]: standalone pgjdbc/Hibernate/Liquibase tests (accept/clients/pgjdbc-real) against fresh staskd instances.
# JDK 17 in a pinned container, Gradle 8.14.4 from the checksum-verified zip (scripts/crmdocker/setup.sh), nothing installed on the host.
# env: BIN (staskd dir, default /tmp/jdbcreal-bin), RUN (default ~/jdbcreal-run), TESTS (gradle --tests filter)
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd); PROJ=$HERE/../../accept/clients/pgjdbc-real
R=${RUN:-$HOME/jdbcreal-run}; . "$R/env.sh"; BIN=${BIN:-/tmp/jdbcreal-bin}
IMG=eclipse-temurin:17.0.13_11-jdk; W=$R/pj; mkdir -p "$W"
psqlc() { docker run --rm --network host -e PGPASSWORD="$CRM_DB_PASSWORD" -e PGSSLMODE=disable postgres:16 psql -X -At -h 127.0.0.1 -p "$1" -U app stask -c "$2" 2>/dev/null; }
gradle() { # gradle <env...> -- args
  mkdir -p "$R/gradle-home"
  [ -d "$R/gradle-home/dist/gradle-8.14.4" ] || docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp -v "$R/gradle-home:/gh" -v "$R/dist:/dist:ro" "$IMG" sh -c 'mkdir -p /gh/dist && cd /gh/dist && jar xf /dist/gradle-8.14.4-bin.zip'
  docker run --rm --network host --user "$(id -u):$(id -g)" -e HOME=/tmp -e GRADLE_USER_HOME=/gh "$@" -v "$R/gradle-home:/gh" -v "$PROJ:/p" -v "$R/crm/src/main/resources:/crmres:ro" -w /p "$IMG" \
    sh /gh/dist/gradle-8.14.4/bin/gradle --no-daemon test --continue ${TESTS:+--tests "$TESTS"} 2>&1
}
start1() { # name pgport httpport dir [extra...]
  local n=$1 pg=$2 ht=$3 d=$4; shift 4; rm -rf "$d"; mkdir -p "$d"
  nohup setsid "$BIN/staskd" -dir "$d" -addr 127.0.0.1:$ht -pg-addr 127.0.0.1:$pg -pg-password "$CRM_DB_PASSWORD" -pg-auth scram -segment-mb 8 -prezero=false "$@" </dev/null >"$d.log" 2>&1 &
  echo $! > "$d.pid"
}
stopall() { for f in "$W"/*.pid; do [ -f "$f" ] && kill "$(cat "$f")" 2>/dev/null; rm -f "$f"; done; }
trap stopall EXIT
mode=${1:-all}
if [ "$mode" = pg ]; then # reference run on PostgreSQL 16: a failure that also happens here is a bug in the test, not in STASK
  docker rm -f jdbcreal-refpg >/dev/null 2>&1; docker run -d --name jdbcreal-refpg -p 127.0.0.1:28835:5432 -e POSTGRES_USER=app -e POSTGRES_PASSWORD="$CRM_DB_PASSWORD" -e POSTGRES_DB=stask postgres:16 >/dev/null
  until docker exec jdbcreal-refpg pg_isready -U app -d stask -h 127.0.0.1 >/dev/null 2>&1; do sleep 1; done; sleep 2
  gradle -e STASK_URL=jdbc:postgresql://127.0.0.1:28835/stask -e STASK_PASSWORD="$CRM_DB_PASSWORD" | tee "$W/pg.out" | grep -E "PASSED|FAILED|SKIPPED|BUILD|tests completed"
  docker rm -f jdbcreal-refpg >/dev/null
fi
if [ "$mode" = single ] || [ "$mode" = all ]; then
  start1 s 28830 28831 "$W/single"; sleep 3
  start1 lb 28832 28833 "$W/lbdb"; sleep 3
  echo "== single-node tests"
  gradle -e STASK_URL=jdbc:postgresql://127.0.0.1:28830/stask -e STASK_PASSWORD="$CRM_DB_PASSWORD" -e CRM_RESOURCES=/crmres | tee "$W/single.out" | grep -E "PASSED|FAILED|SKIPPED|BUILD|tests completed" 
  stopall
fi
if [ "$mode" = cluster ] || [ "$mode" = all ]; then
  peers=""; for i in 1 2 3; do peers="$peers${peers:+,}$i=127.0.0.1:$((28844+i-1))@127.0.0.1:$((28841+i-1))"; done
  for i in 1 2 3; do start1 n$i $((28851+i-1)) $((28841+i-1)) "$W/n$i" -gc=false -raft-id $i -raft-listen 127.0.0.1:$((28844+i-1)) -raft-peers "$peers"; done
  echo "== waiting for a leader"; leader=""
  for k in $(seq 1 60); do for i in 1 2 3; do [ "$(psqlc $((28851+i-1)) 'show transaction_read_only')" = off ] && leader=$i; done; [ -n "$leader" ] && break; sleep 1; done
  echo "leader=node$leader"
  CL=127.0.0.1:28851,127.0.0.1:28852,127.0.0.1:28853
  TESTS='stask.ClusterTest' gradle -e STASK_URL=jdbc:postgresql://127.0.0.1:28851/stask -e STASK_PASSWORD="$CRM_DB_PASSWORD" -e STASK_CLUSTER=$CL -e STASK_PHASE=1 | tee "$W/cluster1.out" | grep -E "PASSED|FAILED|SKIPPED|BUILD"
  echo "== kill -9 leader node$leader"; kill -9 "$(cat "$W/n$leader.pid")"; rm -f "$W/n$leader.pid"
  TESTS='stask.ClusterTest' gradle -e STASK_URL=jdbc:postgresql://127.0.0.1:28851/stask -e STASK_PASSWORD="$CRM_DB_PASSWORD" -e STASK_CLUSTER=$CL -e STASK_PHASE=2 | tee "$W/cluster2.out" | grep -E "PASSED|FAILED|SKIPPED|BUILD"
  stopall
fi
