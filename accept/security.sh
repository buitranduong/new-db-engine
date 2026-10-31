#!/bin/bash
# Real-client security checks of the three network ports of staskd: PG wire
# (TLS + SCRAM), HTTP (TLS, bearer tokens, mTLS) and replication (TLS, mTLS,
# shared secret), plus the start-up refusals of non-loopback listeners with no
# authentication. Clients: pgx (accept/e2e ping), psql 16 (docker postgres:16
# on the host network, or a local psql), curl, and staskd -backup-from /
# -replica-of for the replication port, plus (docs/22, docs/23) the audit trail
# (hash chain, tamper detection), encrypted backups (wrong key, modified file)
# and a 3-node Raft cluster whose Raft port needs mTLS + secret and whose
# membership endpoint (POST /v1/admin/cluster/members) needs a client
# certificate and a token. Each case says what must be accepted
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
printf %s "audit-key-0123456789abcdef-e2e" > $S/audit.key
C=$CERTS

# --- A: everything on, strict ---
start A $S/A -segment-mb 4 -addr 127.0.0.1:$P -pg-addr 127.0.0.1:$((P+1)) -repl-addr 127.0.0.1:$((P+2)) \
  -pg-password "$PW" -pg-auth scram -pg-tls-cert $C/server.crt -pg-tls-key $C/server.key -pg-tls-require \
  -http-tls-cert $C/server.crt -http-tls-key $C/server.key -http-token tok-rw -http-token-ro tok-ro \
  -repl-secret-file $E2E_DIR/repl.secret -repl-tls-cert $C/server.crt -repl-tls-key $C/server.key -repl-tls-ca $C/ca.crt -repl-tls-client-auth -repl-require-secure \
  -audit-dir $S/auditA -audit-key-file $S/audit.key -audit-log all -audit-max-mb 1
PA=$LAST_PID
# --- B: certificate for another name (verify-full must refuse it) ---
start B $S/B -segment-mb 4 -addr 127.0.0.1:$((P+10)) -pg-addr 127.0.0.1:$((P+11)) -repl-addr 127.0.0.1:$((P+12)) \
  -pg-password pw -pg-auth scram -pg-tls-cert $C/wrongname.crt -pg-tls-key $C/wrongname.key \
  -repl-secret-file $E2E_DIR/repl.secret -repl-tls-cert $C/wrongname.crt -repl-tls-key $C/wrongname.key -repl-tls-ca $C/ca.crt
# --- Cc: replication with a secret only, no TLS ---
start Cc $S/Cc -segment-mb 4 -addr 127.0.0.1:$((P+20)) -pg-addr 127.0.0.1:$((P+21)) -repl-addr 127.0.0.1:$((P+22)) -repl-secret-file $E2E_DIR/repl.secret -pg-auth password -pg-password pw
# --- D: HTTP with client certificates as the only authentication ---
start D $S/D -segment-mb 4 -addr 127.0.0.1:$((P+30)) -pg-addr "" -http-tls-cert $C/server.crt -http-tls-key $C/server.key -http-tls-client-ca $C/ca.crt
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
start R1 $S/R1 -segment-mb 4 -addr 127.0.0.1:$((P+50)) -pg-addr 127.0.0.1:$((P+51)) -replica-of $RC -repl-secret-file $E2E_DIR/repl.secret
R1=$LAST_PID
RU="postgres://app:pw@127.0.0.1:$((P+51))/stask?sslmode=disable"
for i in $(seq 100); do [ "$($BIN/e2e ping -url "$RU" -q 'select count(*) from t' 2>/dev/null)" = 3 ] && break; sleep 0.1; done
check "replica with the right secret has the 3 rows" test "$($BIN/e2e ping -url "$RU" -q 'select count(*) from t' 2>/dev/null)" = 3
stop_pid $R1
start R2 $S/R2 -segment-mb 4 -addr 127.0.0.1:$((P+52)) -pg-addr 127.0.0.1:$((P+53)) -replica-of $RC -repl-secret-file $E2E_DIR/repl.wrong
R2=$LAST_PID; sleep 3
refuse "replica with the wrong secret gets no data" $BIN/e2e ping -url "postgres://app:pw@127.0.0.1:$((P+53))/stask?sslmode=disable" -q 'select count(*) from t'
alive $R2 && stop_pid $R2

log "== audit trail (A: -audit-log all, keyed chain) =="
AD=$S/auditA
sleep 1.2     # the audit writer syncs read/write records within a second
check  "audit verify: the chain of the live log is intact"        $BIN/staskctl audit verify -dir $AD -key-file $S/audit.key
refuse "audit verify without the key of a keyed log"              $BIN/staskctl audit verify -dir $AD
nfail=$($BIN/staskctl audit query -dir $AD -ev connect.fail -json | wc -l)
[ "$nfail" -ge 28 ] && ok "audit holds the failed logins ($nfail connect.fail records)" || bad "audit has only $nfail connect.fail records"
$BIN/staskctl audit query -dir $AD -ev connect.ok -json | grep -q '"user":"app"' && ok "audit holds the successful logins" || bad "no connect.ok for app"
$BIN/staskctl audit query -dir $AD -ev connect.fail -json | grep -q 'password authentication failed' && ok "audit gives the reason of a failed login" || bad "no reason in connect.fail"
grep -rq 'wrong\|bad1' $AD && bad "a wrong password text reached the audit files" || ok "no password text in the audit files"
PSQLENV=(PGPASSWORD="$PW" PGSSLMODE=verify-full PGSSLROOTCERT=$C/ca.crt)
check "DDL + data through psql for the audit and backup cases" psql_run $((P+1)) -c "create table sims(id int primary key, msisdn text)" -c "insert into sims values (1,'MSISDN-84901234567-MARKER')" -c "select * from sims" -c "create role auditor password 'aud1tor-pw'" -c "grant select on sims to auditor"
sleep 1.2
$BIN/staskctl audit query -dir $AD -ev ddl -json | grep -q 'CREATE TABLE' && ok "audit holds the CREATE TABLE" || bad "no ddl record"
$BIN/staskctl audit query -dir $AD -ev role -json | grep -q '"cmd":"GRANT"' && ok "audit holds the GRANT" || bad "no GRANT record"
$BIN/staskctl audit query -dir $AD -ev sql.write -json | grep -q '"obj":"sims"' && ok "audit holds the write to sims (session logging)" || bad "no sql.write record"
grep -rq 'aud1tor-pw' $AD && bad "a role password reached the audit files" || ok "the CREATE ROLE password is not in the audit files"
log "     audit files: $(ls $AD | grep -c 'audit-')"
# tamper evidence on a copy
rm -rf $S/auditcopy; cp -r $AD $S/auditcopy
f=$(ls $S/auditcopy/audit-*.log | head -1)
sed -i '3s/"ev":"[a-z.]*"/"ev":"zzz"/' $f
refuse "audit verify detects an edited record"                    $BIN/staskctl audit verify -dir $S/auditcopy -key-file $S/audit.key
rm -rf $S/auditcopy; cp -r $AD $S/auditcopy; f=$(ls $S/auditcopy/audit-*.log | head -1)
sed -i '5d' $f
refuse "audit verify detects a deleted record"                    $BIN/staskctl audit verify -dir $S/auditcopy -key-file $S/audit.key
rm -rf $S/auditcopy; cp -r $AD $S/auditcopy; f=$(ls $S/auditcopy/audit-*.log | tail -1)
head -n -3 $f > $f.cut && mv $f.cut $f
refuse "audit verify detects a cut-off tail (audit.head)"         $BIN/staskctl audit verify -dir $S/auditcopy -key-file $S/audit.key
rm -rf $S/auditcopy

log "== encrypted backup (A) =="
BKEY=$S/backup.key
check  "staskctl keygen writes a key file (0600)"                 $BIN/staskctl keygen -out $BKEY
refuse "staskctl keygen does not overwrite a key"                 $BIN/staskctl keygen -out $BKEY
[ "$(stat -c %a $BKEY)" = 600 ] && ok "key file mode 600" || bad "key file mode $(stat -c %a $BKEY)"
$BIN/staskctl keygen -out $S/other.key > /dev/null 2>&1
rm -rf $S/ebk $S/pbk
SEC=(-repl-secret-file $E2E_DIR/repl.secret -repl-tls-ca $C/ca.crt -repl-tls-cert $C/client.crt -repl-tls-key $C/client.key)
check  "plain backup (premise: the data is readable in it)"       $BIN/staskctl backup -from $RA -out $S/pbk "${SEC[@]}"
grep -rqa 'MSISDN-84901234567-MARKER' $S/pbk && ok "premise: the plain backup contains the data" || bad "premise: the marker is not in the plain backup"
check  "encrypted backup"                                         $BIN/staskctl backup -from $RA -out $S/ebk "${SEC[@]}" -key-file $BKEY
EB=$(ls -d $S/ebk/*/ | head -1)
grep -rqa 'MSISDN-84901234567-MARKER' $EB && bad "the encrypted backup contains the data in clear" || ok "the encrypted backup holds no plaintext marker"
grep -rqa "$(cat $BKEY)" $EB && bad "the key is inside the backup" || ok "the key is not inside the backup"
check  "verify the encrypted backup with the key"                 $BIN/staskctl verify -backup $EB -key-file $BKEY
refuse "verify with another key (clean refusal)"                  $BIN/staskctl verify -backup $EB -key-file $S/other.key
$BIN/staskctl verify -backup $EB -key-file $S/other.key 2>&1 | grep -q 'wrong encryption key' && ok "the refusal says: wrong encryption key" || bad "unclear message for a wrong key"
refuse "verify with no key"                                       env -u STASK_BACKUP_KEY $BIN/staskctl verify -backup $EB
check  "verify with the key from \$STASK_BACKUP_KEY"              env STASK_BACKUP_KEY="$(cat $BKEY)" $BIN/staskctl verify -backup $EB
cp -r $EB $S/ebk-bad; bf=$(ls $S/ebk-bad/wal/* | head -1); printf '\xff' | dd of=$bf bs=1 seek=100 conv=notrunc 2>/dev/null
refuse "verify detects a modified encrypted file"                 $BIN/staskctl verify -backup $S/ebk-bad -key-file $BKEY
rm -rf $S/ebk-bad $S/ebk-restored
refuse "restore with the wrong key leaves nothing"                $BIN/staskctl restore -backup $EB -dir $S/ebk-restored -key-file $S/other.key
check  "restore with the key"                                     $BIN/staskctl restore -backup $EB -dir $S/ebk-restored -key-file $BKEY
rm -rf $S/ebk-restored $S/pbk $S/ebk

log "== Raft cluster: mTLS + secret on the Raft port, mTLS + token on the membership endpoint =="
CP=$((P+100)); hp() { echo $((CP+$1)); }; rp() { echo $((CP+10+$1)); }
PEERS="1=127.0.0.1:$(rp 1)@127.0.0.1:$(hp 1),2=127.0.0.1:$(rp 2)@127.0.0.1:$(hp 2),3=127.0.0.1:$(rp 3)@127.0.0.1:$(hp 3)"
CHTTP=(-http-tls-cert $C/server.crt -http-tls-key $C/server.key -http-tls-client-ca $C/ca.crt -http-token tok-rw -http-token-ro tok-ro)
CRAFT=(-repl-tls-cert $C/server.crt -repl-tls-key $C/server.key -repl-tls-ca $C/ca.crt -repl-tls-client-auth -repl-require-secure)
node() { # id secretfile peers [extra...]
  local i=$1 sec=$2 peers=$3; shift 3
  start N$i $S/N$i -segment-mb 4 -gc=false -prezero=false -addr 127.0.0.1:$(hp $i) -pg-addr "" -raft-id $i -raft-listen 127.0.0.1:$(rp $i) -raft-peers "$peers" \
    "${CHTTP[@]}" "${CRAFT[@]}" -repl-secret-file $sec "$@"; }
for i in 1 2 3; do node $i $E2E_DIR/repl.secret "$PEERS"; done
CT=(-ca $C/ca.crt -cert $C/client.crt -key $C/client.key)
leader_ready() { for i in $(seq 150); do for n in 1 2 3; do
  [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 --cacert $C/ca.crt --cert $C/client.crt --key $C/client.key -H 'Authorization: Bearer tok-rw' "https://127.0.0.1:$(hp $n)/v1/ready?write=1")" = 200 ] && return 0; done; sleep 0.2; done; return 1; }
check "the 3-node cluster elects a leader (mTLS + secret on the Raft port)" leader_ready
URLS=https://127.0.0.1:$(hp 1),https://127.0.0.1:$(hp 2),https://127.0.0.1:$(hp 3)
add() { # id extra-ctl-args... : cluster add against the three voters (stdin closed, short retries)
  local id=$1; shift; timeout 40 $BIN/staskctl cluster add -http $URLS -id $id -raft 127.0.0.1:$(rp $id) "$@"; }
addtok() { local t=$1; shift; STASK_TOKEN=$t add "$@"; }
rawraft() { python3 - "$(rp 1)" <<'PY'
import socket,sys
s=socket.create_connection(("127.0.0.1",int(sys.argv[1])),timeout=3)
s.sendall(b"\x00\x00\x00\x09H\x02\x00\x00\x00\x00\x00\x00\x00")
s.settimeout(3)
try: d=s.recv(200)
except Exception: d=b""
sys.exit(1 if (d and b"TLS" not in d and b"requires" not in d) else 0)
PY
}
check  "a plain TCP client gets no Raft service on the Raft port"  rawraft
refuse "membership: valid token, no client certificate"           env STASK_TOKEN=tok-rw timeout 40 $BIN/staskctl cluster add -http $URLS -id 4 -raft 127.0.0.1:$(rp 4) -ca $C/ca.crt
refuse "membership: valid token, certificate from a foreign CA"   addtok tok-rw 4 -ca $C/ca.crt -cert $C/evil-client.crt -key $C/evil-client.key
refuse "membership: client certificate, no token"                 addtok "" 4 "${CT[@]}"
refuse "membership: client certificate, wrong token"              addtok nope 4 "${CT[@]}"
refuse "membership: client certificate, read-only token (403)"    addtok tok-ro 4 "${CT[@]}"
# node 5 has a different Raft secret: the leader cannot reach it with the cluster secret
PEERS5="$PEERS,5=127.0.0.1:$(rp 5)@127.0.0.1:$(hp 5)"
node 5 $E2E_DIR/repl.wrong "$PEERS5" -raft-join
wait_port $(rp 5) || bad "node 5 did not listen"
refuse "membership: new node with another Raft secret is not added" addtok tok-rw 5 "${CT[@]}"
grep -q "connection refused" $E2E_DIR/last.out && bad "node 5 was refused because it did not listen, not because of its secret" || ok "the refusal came from the Raft handshake, not from a closed port"
# node 4 with the right secret joins
PEERS4="$PEERS,4=127.0.0.1:$(rp 4)@127.0.0.1:$(hp 4)"
node 4 $E2E_DIR/repl.secret "$PEERS4" -raft-join
wait_port $(rp 4) || bad "node 4 did not listen"
check  "membership: client certificate + admin token adds node 4"  addtok tok-rw 4 "${CT[@]}"
ready4() { for i in $(seq 150); do [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 --cacert $C/ca.crt --cert $C/client.crt --key $C/client.key -H 'Authorization: Bearer tok-rw' "https://127.0.0.1:$(hp 4)/v1/ready")" = 200 ] && return 0; sleep 0.2; done; return 1; }
check  "node 4 became ready after joining (it received the snapshot over the secured Raft port)" ready4
check  "the cluster still has a writable leader"                   leader_ready

log "== start-up refusals (non-loopback listener without authentication) =="
NR=0; S_MSG=1
startup() { # label refuse|run flags...   run staskd for 3 s; exit before then = refused
  local l=$1 want=$2; shift 2; NR=$((NR+1))
  rm -rf $S/su$NR
  timeout 3 $BIN/staskd -segment-mb 4 -dir $S/su$NR -addr 127.0.0.1:$((P+60+NR)) -pg-addr "" "$@" > $S/su$NR.log 2>&1; local rc=$?
  local got=run; [ $rc != 124 ] && got=refuse
  if [ $got = $want ]; then ok "$l -> $got${S_MSG:+ ($(grep -v "http api" $S/su$NR.log | tail -1 | sed "s/^[0-9\/: ]* //" | cut -c1-110))}"; else bad "$l -> $got, want $want (rc=$rc)"; tail -3 $S/su$NR.log; fi
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
