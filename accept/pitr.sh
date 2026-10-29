#!/bin/bash
# P5.4 end to end: point-in-time recovery of a leader that is being written.
#
#   leader: -archive-dir, TLS+mTLS+shared secret on the replication port
#           (-repl-require-secure), TLS+SCRAM on the PG port, TLS+token on HTTP;
#   one client process (accept/e2e write, 4 connections) inserts, updates and
#   deletes continuously and logs every acknowledged transaction;
#   a base backup is taken over the network (-backup-from, mTLS) WHILE the
#   client writes; writing goes on until the archive holds ARCH_MB;
#   the leader's own record of each commit (CDC: commit LSN and time) gives the
#   exact content a recovery to a target must have. Two restores:
#     -recovery-target-time and -recovery-target-lsn; the table must equal the
#   expected one row for row (none lost, none extra), and the restored
#   database must accept writes.
#
#   ARCH_MB=1200 E2E_DIR=~/e2e-data accept/pitr.sh        (results in $E2E_DIR/pitr.txt)
. "$(dirname "$0")/lib.sh"
ARCH_MB=${ARCH_MB:-1200}
PORT=$E2E_BASE
mkdir -p $E2E_DIR; cd $ROOT
exec > >(tee $E2E_DIR/pitr.txt) 2>&1
trap 'stop_all' EXIT
need_free $((ARCH_MB * 5 / 2))
build
log "disk free $(free_mb) MB, archive target $ARCH_MB MB"
rm -rf $E2E_DIR/{leader,arch,backup,r1,r2,*.log,w.log,m.txt}; mkcerts
echo -n tok-e2e-pitr > $E2E_DIR/http.token
PGURL="postgres://app:pw@127.0.0.1:$((PORT+1))/stask?sslmode=verify-full&sslrootcert=$CERTS/ca.crt"
PSQLENV=(PGPASSWORD=pw PGSSLMODE=verify-full)

start leader $E2E_DIR/leader -addr 127.0.0.1:$PORT -pg-addr 127.0.0.1:$((PORT+1)) -repl-addr 127.0.0.1:$((PORT+2)) \
  -segment-mb 16 -archive-dir $E2E_DIR/arch \
  -repl-secret-file $E2E_DIR/repl.secret -repl-tls-cert $CERTS/server.crt -repl-tls-key $CERTS/server.key \
  -repl-tls-ca $CERTS/ca.crt -repl-tls-client-auth -repl-require-secure \
  -pg-password pw -pg-auth scram -pg-tls-cert $CERTS/server.crt -pg-tls-key $CERTS/server.key -pg-tls-require \
  -http-tls-cert $CERTS/server.crt -http-tls-key $CERTS/server.key -http-token tok-e2e-pitr
LEADER=$LAST_PID
wait_port $((PORT+1)) || { bad "leader did not start"; tail $E2E_DIR/leader.log; exit 1; }

cat > $E2E_DIR/init.sql <<SQL
create table ev(id bigint primary key, w int not null, ver int not null, payload text not null);
select pg_create_logical_replication_slot('pitr','test_decoding');
SQL
check "psql 16 over verify-full TLS + SCRAM: create table and slot" psql_run $((PORT+1)) -f $E2E_DIR/init.sql

$BIN/e2e write -url "$PGURL" -log $E2E_DIR/w.log -size 1000 -batch 20 -workers 4 2> $E2E_DIR/writer.log &
WPID=$!
arch_mb() { du -sm $E2E_DIR/arch 2>/dev/null | cut -f1; }
# base backup while the client writes, once the archive has grown a little
while [ "$(arch_mb)" -lt $((ARCH_MB / 8)) ]; do sleep 0.5; done
T0=$(date +%s.%N)
if $BIN/staskd -dir $E2E_DIR/backup -backup-from 127.0.0.1:$((PORT+2)) -repl-secret-file $E2E_DIR/repl.secret \
     -repl-tls-ca $CERTS/ca.crt -repl-tls-cert $CERTS/client.crt -repl-tls-key $CERTS/client.key > $E2E_DIR/backup.log 2>&1; then
  ok "base backup over TLS+mTLS+secret while writing ($(du -sm $E2E_DIR/backup | cut -f1) MB, $(echo "$(date +%s.%N) - $T0" | bc) s): $(grep 'base backup' $E2E_DIR/backup.log | sed 's/.*repl: //')"
else bad "base backup"; cat $E2E_DIR/backup.log; fi
while [ "$(arch_mb)" -lt $ARCH_MB ]; do sleep 1; alive $WPID || break; done
kill -INT $WPID; wait $WPID; cat $E2E_DIR/writer.log | tail -2
log "archive $(arch_mb) MB, $(ls $E2E_DIR/arch | wc -l) segments; leader dir $(du -sm $E2E_DIR/leader | cut -f1) MB"

# the leader's record of each commit
check "cdcmap" $BIN/e2e cdcmap -url "$PGURL" -slot pitr -out $E2E_DIR/m.txt
cat $E2E_DIR/last.out
check "leader holds exactly every acknowledged transaction" $BIN/e2e verify -url "$PGURL" -log $E2E_DIR/w.log -mode acked
read TLSN _ < <($BIN/e2e pick -cdcmap $E2E_DIR/m.txt -frac 0.45)
read _ TTS < <($BIN/e2e pick -cdcmap $E2E_DIR/m.txt -frac 0.70)
log "targets: lsn=$TLSN time=$TTS"
stop_pid $LEADER; rm -rf $E2E_DIR/leader

restore() { # NAME ARGS... : restore a copy of the backup, verify, check it takes writes
  local n=$1 label=$2; shift 2
  rm -rf $E2E_DIR/$n; cp -a $E2E_DIR/backup $E2E_DIR/$n
  local t0=$(date +%s.%N)
  start $n $E2E_DIR/$n -addr 127.0.0.1:$((PORT+10)) -pg-addr 127.0.0.1:$((PORT+11)) -segment-mb 16 -restore-archive $E2E_DIR/arch "$@"
  local p=$LAST_PID
  if wait_port $((PORT+11)); then
    log "restore $label took $(echo "$(date +%s.%N) - $t0" | bc) s: $(grep -E 'recovery' $E2E_DIR/$n.log | head -2 | tr '\n' ' ')"
    local u="postgres://app@127.0.0.1:$((PORT+11))/stask?sslmode=disable"
    check "restore to $label: table equals the expected content" $BIN/e2e verify -url "$u" -log $E2E_DIR/w.log -mode exact -cdcmap $E2E_DIR/m.txt "${VERIFY[@]}"
    cat $E2E_DIR/last.out
    check "restored database accepts writes" psql_run $((PORT+11)) -c "insert into ev values (-1, 0, 0, 'after recovery')" -c "select count(*) from ev"
  else bad "restore to $label did not start"; tail -5 $E2E_DIR/$n.log; fi
  stop_pid $p; rm -rf $E2E_DIR/$n
}
PSQLENV=(PGSSLMODE=disable)
VERIFY=(-ts $TTS);  restore r1 "time $TTS" -recovery-target-time $TTS
VERIFY=(-lsn $TLSN); restore r2 "lsn $TLSN" -recovery-target-lsn $TLSN
log "result: $FAILS failure(s)"
[ $FAILS = 0 ]
