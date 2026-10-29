#!/bin/bash
# Real-client security checks of the three network ports of staskd: PG wire
# (TLS + SCRAM), HTTP (TLS, bearer tokens, mTLS) and replication (TLS, mTLS,
# shared secret), plus the start-up refusals of non-loopback listeners with no
# authentication. Clients: pgx (accept/e2e ping), psql 16 (docker postgres:16
# on the host network, or a local psql), curl, and staskd -backup-from /
# -replica-of for the replication port. Each case says what must be accepted
# and what must be refused; the leader must stay healthy throughout.
#
#   accept/security.sh        (results in $E2E_DIR/security.txt)
. "$(dirname "$0")/lib.sh"
mkdir -p $E2E_DIR; cd $ROOT
exec > >(tee $E2E_DIR/security.txt) 2>&1
trap 'stop_all' EXIT
need_free 600
build
S=$E2E_DIR/sec; rm -rf $S; mkdir -p $S; mkcerts
P=$((E2E_BASE + 40))      # A: P http, P+1 pg, P+2 repl
PW='p@ss w:rd/%é"x'       # awkward on purpose: SASLprep, URL and shell quoting
echo -n tok-rw > $S/rw.token
C=$CERTS

# --- A: everything on, strict ---
start A $S/A -addr 127.0.0.1:$P -pg-addr 127.0.0.1:$((P+1)) -repl-addr 127.0.0.1:$((P+2)) \
  -pg-password "$PW" -pg-auth scram -pg-tls-cert $C/server.crt -pg-tls-key $C/server.key -pg-tls-require \
  -http-tls-cert $C/server.crt -http-tls-key $C/server.key -http-token tok-rw -http-token-ro tok-ro \
  -repl-secret-file $E2E_DIR/repl.secret -repl-tls-cert $C/server.crt -repl-tls-key $C/server.key -repl-tls-ca $C/ca.crt -repl-tls-client-auth -repl-require-secure
PA=$LAST_PID
# --- B: certificate for another name (verify-full must refuse it) ---
start B $S/B -addr 127.0.0.1:$((P+10)) -pg-addr 127.0.0.1:$((P+11)) -repl-addr 127.0.0.1:$((P+12)) \
  -pg-password pw -pg-auth scram -pg-tls-cert $C/wrongname.crt -pg-tls-key $C/wrongname.key \
  -repl-secret-file $E2E_DIR/repl.secret -repl-tls-cert $C/wrongname.crt -repl-tls-key $C/wrongname.key -repl-tls-ca $C/ca.crt
# --- Cc: replication with a secret only, no TLS ---
start Cc $S/Cc -addr 127.0.0.1:$((P+20)) -pg-addr 127.0.0.1:$((P+21)) -repl-addr 127.0.0.1:$((P+22)) -repl-secret-file $E2E_DIR/repl.secret -pg-auth password -pg-password pw
# --- D: HTTP with client certificates as the only authentication ---
start D $S/D -addr 127.0.0.1:$((P+30)) -pg-addr "" -http-tls-cert $C/server.crt -http-tls-key $C/server.key -http-tls-client-ca $C/ca.crt
for q in $((P+1)) $((P+11)) $((P+21)); do wait_port $q || { bad "server on $q did not start"; tail -3 $S/../A.log; exit 1; }; done
wait_port $((P+30))

url() { # port user pass sslmode rootcert
  python3 - "$@" <<'PY'
import sys,urllib.parse as u
port,user,pw,mode,root=sys.argv[1:6]
print("postgres://%s:%s@127.0.0.1:%s/stask?sslmode=%s&sslrootcert=%s"%(user,u.quote(pw,safe=''),port,mode,root))
PY
}
pgxping() { $BIN/e2e ping -url "$(url "$@")"; }
psqlping() { # port pass sslmode rootcert
  PSQLENV=(PGPASSWORD="$2" PGSSLMODE="$3" PGSSLROOTCERT="${4//$E2E_DIR//d}"); psql_run $1 -c "select 1"; }

log "== PG wire (A: SCRAM, TLS required, awkward password) =="
check   "pgx   correct password, verify-full"            pgxping $((P+1)) app "$PW" verify-full $C/ca.crt
check   "psql  correct password, verify-full"            psqlping $((P+1)) "$PW" verify-full $C/ca.crt
check   "pgx   any user name with the shared password"   pgxping $((P+1)) someoneelse "$PW" verify-full $C/ca.crt
refuse  "pgx   wrong password"                           pgxping $((P+1)) app wrong verify-full $C/ca.crt
refuse  "psql  wrong password"                           psqlping $((P+1)) wrong verify-full $C/ca.crt
refuse  "psql  empty password"                           psqlping $((P+1)) "" verify-full $C/ca.crt
refuse  "pgx   password differing in the last character" pgxping $((P+1)) app "${PW%x}y" verify-full $C/ca.crt
refuse  "pgx   plaintext (sslmode=disable) with -pg-tls-require" pgxping $((P+1)) app "$PW" disable $C/ca.crt
refuse  "psql  plaintext (sslmode=disable)"              psqlping $((P+1)) "$PW" disable $C/ca.crt
refuse  "pgx   verify-full against a foreign CA"         pgxping $((P+1)) app "$PW" verify-full $C/evil-ca.crt
check   "pgx   sslmode=require (encrypts, no verification)" pgxping $((P+1)) app "$PW" require $C/ca.crt
for i in $(seq 25); do pgxping $((P+1)) app "bad$i" verify-full $C/ca.crt > /dev/null 2>&1; done
check   "pgx   still serves after 25 failed logins"      pgxping $((P+1)) app "$PW" verify-full $C/ca.crt
log "-- B: server certificate is for another name"
refuse  "pgx   verify-full (certificate name mismatch)"  pgxping $((P+11)) app pw verify-full $C/ca.crt
refuse  "psql  verify-full (certificate name mismatch)"  psqlping $((P+11)) pw verify-full $C/ca.crt
check   "pgx   sslmode=require still works"              pgxping $((P+11)) app pw require $C/ca.crt
log "-- Cc: -pg-auth password (cleartext inside the protocol), no TLS"
check   "pgx   correct password"                         pgxping $((P+21)) app pw disable $C/ca.crt
refuse  "pgx   wrong password"                           pgxping $((P+21)) app nope disable $C/ca.crt
refuse  "psql  wrong password"                           psqlping $((P+21)) nope disable $C/ca.crt

log "== HTTP API (A: TLS + tokens) =="
code() { curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$@" 2>/dev/null; }
want() { # label want-code curl-args...
  local l=$1 w=$2; shift 2; local g; g=$(code "$@")
  [ "$g" = "$w" ] && ok "$l -> $g" || bad "$l -> $g, want $w"; }
H=https://127.0.0.1:$P
want "GET /v1/status, no token"                      401 --cacert $C/ca.crt $H/v1/status
want "GET /v1/status, wrong token"                   401 --cacert $C/ca.crt -H "Authorization: Bearer nope" $H/v1/status
want "GET /v1/status, read-write token"              200 --cacert $C/ca.crt -H "Authorization: Bearer tok-rw" $H/v1/status
want "GET /v1/status, read-only token"               200 --cacert $C/ca.crt -H "Authorization: Bearer tok-ro" $H/v1/status
want "POST /v1/write, read-only token"               403 --cacert $C/ca.crt -H "Authorization: Bearer tok-ro" -X POST -d '{}' $H/v1/write
want "POST /v1/admin/checkpoint, read-only token"    403 --cacert $C/ca.crt -H "Authorization: Bearer tok-ro" -X POST $H/v1/admin/checkpoint
want "POST /v1/admin/checkpoint, no token"           401 --cacert $C/ca.crt -X POST $H/v1/admin/checkpoint
want "POST /v1/admin/checkpoint, read-write token"   200 --cacert $C/ca.crt -H "Authorization: Bearer tok-rw" -X POST $H/v1/admin/checkpoint
want "GET /v1/health, no token (public by design)"   200 --cacert $C/ca.crt $H/v1/health
want "HTTP Basic with the PG password (-http-basic off)" 401 --cacert $C/ca.crt -u "app:$PW" $H/v1/status
want "token in the query string is not accepted"     401 --cacert $C/ca.crt "$H/v1/status?token=tok-rw"
c=$(code http://127.0.0.1:$P/v1/status -H "Authorization: Bearer tok-rw"); [ "$c" != 200 ] && ok "plaintext HTTP to the TLS port -> $c" || bad "plaintext HTTP served a request on the TLS port"
c=$(code --cacert $C/evil-ca.crt -H "Authorization: Bearer tok-rw" $H/v1/status); [ "$c" = 000 ] && ok "client rejects a certificate not signed by its CA (curl: no response)" || bad "foreign CA accepted: $c"
log "-- D: mTLS only"
HD=https://127.0.0.1:$((P+30))
want "GET /v1/status, client certificate from the CA"  200 --cacert $C/ca.crt --cert $C/client.crt --key $C/client.key $HD/v1/status
want "GET /v1/status, no client certificate"           000 --cacert $C/ca.crt $HD/v1/status
want "GET /v1/status, client certificate from a foreign CA" 000 --cacert $C/ca.crt --cert $C/evil-client.crt --key $C/evil-client.key $HD/v1/status

log "== Replication port (A: TLS + mTLS + secret + -repl-require-secure) =="
RA=127.0.0.1:$((P+2)); n=0
bk() { # leader args... : a base backup into a fresh directory
  local l=$1; shift; n=$((n+1)); rm -rf $S/bk$n
  $BIN/staskd -dir $S/bk$n -backup-from $l "$@"; local rc=$?; rm -rf $S/bk$n; return $rc; }
GOOD=(-repl-secret-file $E2E_DIR/repl.secret -repl-tls-ca $C/ca.crt -repl-tls-cert $C/client.crt -repl-tls-key $C/client.key)
check  "backup with secret + CA + client certificate"          bk $RA "${GOOD[@]}"
refuse "backup with the wrong secret"                          bk $RA -repl-secret-file $E2E_DIR/repl.wrong -repl-tls-ca $C/ca.crt -repl-tls-cert $C/client.crt -repl-tls-key $C/client.key
refuse "backup with no secret"                                 bk $RA -repl-tls-ca $C/ca.crt -repl-tls-cert $C/client.crt -repl-tls-key $C/client.key
refuse "backup with secret but no TLS at all"                  bk $RA -repl-secret-file $E2E_DIR/repl.secret
refuse "backup with secret + TLS but no client certificate"    bk $RA -repl-secret-file $E2E_DIR/repl.secret -repl-tls-ca $C/ca.crt
refuse "backup with a client certificate from a foreign CA"    bk $RA -repl-secret-file $E2E_DIR/repl.secret -repl-tls-ca $C/ca.crt -repl-tls-cert $C/evil-client.crt -repl-tls-key $C/evil-client.key
refuse "backup with certificate but no secret and no TLS CA"   bk $RA -repl-tls-cert $C/client.crt -repl-tls-key $C/client.key
refuse "backup verifying the leader against a foreign CA"      bk $RA -repl-secret-file $E2E_DIR/repl.secret -repl-tls-ca $C/evil-ca.crt -repl-tls-cert $C/client.crt -repl-tls-key $C/client.key
refuse "backup with nothing configured"                        bk $RA
check  "backup again: the leader survived all of that"         bk $RA "${GOOD[@]}"
log "-- B: the leader's certificate carries another name"
RB=127.0.0.1:$((P+12))
refuse "backup verify-full by address"                         bk $RB -repl-secret-file $E2E_DIR/repl.secret -repl-tls-ca $C/ca.crt
check  "backup with -repl-tls-server-name matching the certificate" bk $RB -repl-secret-file $E2E_DIR/repl.secret -repl-tls-ca $C/ca.crt -repl-tls-server-name other.example
log "-- Cc: leader with a secret and no TLS"
RC=127.0.0.1:$((P+22))
check  "backup with the secret (plaintext, challenge-response)" bk $RC -repl-secret-file $E2E_DIR/repl.secret
refuse "backup with the wrong secret"                          bk $RC -repl-secret-file $E2E_DIR/repl.wrong
refuse "backup with no secret"                                 bk $RC
refuse "replica side -repl-require-secure refuses a leader without TLS" bk $RC -repl-secret-file $E2E_DIR/repl.secret -repl-require-secure

log "-- a real replica (-replica-of)"
psql_run_c() { PSQLENV=(PGPASSWORD=pw PGSSLMODE=disable); psql_run $((P+21)) "$@"; }
check "write on the leader (Cc)" psql_run_c -c "create table t(a int primary key)" -c "insert into t values (1),(2),(3)"
start R1 $S/R1 -addr 127.0.0.1:$((P+50)) -pg-addr 127.0.0.1:$((P+51)) -replica-of $RC -repl-secret-file $E2E_DIR/repl.secret
R1=$LAST_PID
RU="postgres://app:pw@127.0.0.1:$((P+51))/stask?sslmode=disable"
for i in $(seq 100); do [ "$($BIN/e2e ping -url "$RU" -q 'select count(*) from t' 2>/dev/null)" = 3 ] && break; sleep 0.1; done
check "replica with the right secret has the 3 rows" test "$($BIN/e2e ping -url "$RU" -q 'select count(*) from t' 2>/dev/null)" = 3
stop_pid $R1
start R2 $S/R2 -addr 127.0.0.1:$((P+52)) -pg-addr 127.0.0.1:$((P+53)) -replica-of $RC -repl-secret-file $E2E_DIR/repl.wrong
R2=$LAST_PID; sleep 3
refuse "replica with the wrong secret gets no data" $BIN/e2e ping -url "postgres://app:pw@127.0.0.1:$((P+53))/stask?sslmode=disable" -q 'select count(*) from t'
alive $R2 && stop_pid $R2

log "== start-up refusals (non-loopback listener without authentication) =="
NR=0
startup() { # label refuse|run flags...   run staskd for 3 s; exit before then = refused
  local l=$1 want=$2; shift 2; NR=$((NR+1))
  rm -rf $S/su$NR
  timeout 3 $BIN/staskd -dir $S/su$NR -addr 127.0.0.1:$((P+60+NR)) -pg-addr "" "$@" > $S/su$NR.log 2>&1; local rc=$?
  local got=run; [ $rc != 124 ] && got=refuse
  if [ $got = $want ]; then ok "$l -> $got ($(grep -v '^20' $S/su$NR.log | head -1 | cut -c1-100)$(grep -m1 -E 'refus|must|require|set ' $S/su$NR.log | sed 's/^[0-9\/: ]* //' | cut -c1-120))"; else bad "$l -> $got, want $want (rc=$rc)"; tail -3 $S/su$NR.log; fi
  rm -rf $S/su$NR; }
startup "-pg-addr 0.0.0.0, no password"                 refuse -pg-addr 0.0.0.0:$((P+90))
startup "-pg-addr 0.0.0.0 -pg-password"                 run    -pg-addr 0.0.0.0:$((P+90)) -pg-password x -pg-auth scram
startup "-pg-addr 0.0.0.0 -pg-insecure"                 run    -pg-addr 0.0.0.0:$((P+90)) -pg-insecure
startup "-pg-addr 127.0.0.1, no password (trust)"       run    -pg-addr 127.0.0.1:$((P+90))
startup "-pg-addr [::], no password"                    refuse -pg-addr "[::]:$((P+90))"
startup "-pg-addr :PORT (all interfaces), no password"  refuse -pg-addr ":$((P+90))"
startup "-pg-addr 192.168.0.220 (LAN address), no password" refuse -pg-addr "$(hostname -I | awk '{print $1}'):$((P+90))"
startup "-addr 0.0.0.0, no token"                       refuse -addr 0.0.0.0:$((P+91))
startup "-addr 0.0.0.0 -http-token"                     run    -addr 0.0.0.0:$((P+91)) -http-token t
startup "-addr 0.0.0.0 -http-insecure"                  run    -addr 0.0.0.0:$((P+91)) -http-insecure
startup "-repl-addr 0.0.0.0, no secret"                 refuse -repl-addr 0.0.0.0:$((P+92))
startup "-repl-addr 0.0.0.0 -repl-secret-file"          run    -repl-addr 0.0.0.0:$((P+92)) -repl-secret-file $E2E_DIR/repl.secret
startup "-repl-addr 0.0.0.0 -repl-insecure"             run    -repl-addr 0.0.0.0:$((P+92)) -repl-insecure
startup "-pprof 0.0.0.0, no credentials"                refuse -pprof 0.0.0.0:$((P+93))
startup "-pprof 0.0.0.0 -pprof-insecure"                run    -pprof 0.0.0.0:$((P+93)) -pprof-insecure
startup "-pg-tls-cert without key"                      refuse -pg-addr 127.0.0.1:$((P+90)) -pg-tls-cert $C/server.crt
startup "-pg-auth scram without -pg-password"           refuse -pg-addr 127.0.0.1:$((P+90)) -pg-auth scram
startup "-repl-require-secure without secret or TLS"    refuse -repl-addr 127.0.0.1:$((P+92)) -repl-require-secure

check "A is still healthy at the end" pgxping $((P+1)) app "$PW" verify-full $C/ca.crt
log "result: $FAILS failure(s)"
[ $FAILS = 0 ]
