# Shared helpers of the end-to-end acceptance scripts (pitr.sh, kill9.sh,
# security.sh). Source it; it does not run anything.
#
# Safety: a script only ever stops processes it started itself (pids kept in
# $E2E_PIDS; nothing is ever stopped by name or by pattern),
# only uses ports in 20000-20999 and only touches $E2E_DIR.
#
# Environment:
#   E2E_DIR   work directory (default ~/e2e-data; wiped per run by each script)
#   E2E_BASE  first port of the block this script uses (default 20100)
#   BIN       directory with staskd and e2e, built from this tree (default bin-x)
export PATH=$PATH:/usr/local/go/bin
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
E2E_DIR=${E2E_DIR:-$HOME/e2e-data}
E2E_BASE=${E2E_BASE:-20100}
BIN=${BIN:-$ROOT/bin-x}
CERTS=$E2E_DIR/certs
E2E_PIDS=()
FAILS=0

log()  { echo "$(date +%T) $*"; }
ok()   { log "OK   $*"; }
bad()  { log "FAIL $*"; FAILS=$((FAILS+1)); }
check() { # check "label" cmd... : the command must succeed
  local l=$1; shift
  if "$@" > $E2E_DIR/last.out 2>&1; then ok "$l"; else bad "$l"; sed 's/^/     /' $E2E_DIR/last.out | tail -5; fi
}
refuse() { # refuse "label" cmd... : the command must fail
  local l=$1; shift
  if "$@" > $E2E_DIR/last.out 2>&1; then bad "$l (accepted, should be refused)"; else ok "$l (refused: $(tr '\n' ' ' < $E2E_DIR/last.out | cut -c1-110))"; fi
}

build() {
  (cd $ROOT && go build -o $BIN/ ./cmd/... && go build -o $BIN/e2e ./accept/e2e) || { echo "build failed"; exit 2; }
}

free_mb() { df -Pm "$E2E_DIR" | awk 'NR==2{print $4}'; }
need_free() { # MB
  local f=$(free_mb)
  [ "$f" -ge "$1" ] || { echo "only $f MB free in $E2E_DIR, need $1"; exit 2; }
}

mkcerts() { # CA, a server certificate (127.0.0.1, localhost) and a client certificate, both signed by it; plus a foreign CA
  rm -rf $CERTS; mkdir -p $CERTS; (cd $CERTS
  openssl req -x509 -newkey rsa:2048 -nodes -keyout ca.key -out ca.crt -subj "/CN=stask-e2e-ca" -days 3 2>/dev/null
  openssl req -x509 -newkey rsa:2048 -nodes -keyout evil-ca.key -out evil-ca.crt -subj "/CN=evil-ca" -days 3 2>/dev/null
  mk() { # name cn signing-ca
    openssl req -newkey rsa:2048 -nodes -keyout $1.key -out $1.csr -subj "/CN=$2" 2>/dev/null
    printf "subjectAltName=DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth,clientAuth\n" > $1.ext
    openssl x509 -req -in $1.csr -CA $3.crt -CAkey $3.key -CAcreateserial -out $1.crt -days 3 -extfile $1.ext 2>/dev/null; }
  mk server localhost ca; mk client replica1 ca; mk evil-client intruder evil-ca
  # a server certificate for another name, to see verify-full refuse it
  openssl req -newkey rsa:2048 -nodes -keyout wrongname.key -out wrongname.csr -subj "/CN=other" 2>/dev/null
  printf "subjectAltName=DNS:other.example\n" > wrongname.ext
  openssl x509 -req -in wrongname.csr -CA ca.crt -CAkey ca.key -CAcreateserial -out wrongname.crt -days 3 -extfile wrongname.ext 2>/dev/null)
  printf %s "s3cret-repl-e2e" > $E2E_DIR/repl.secret; printf %s "wrong-secret" > $E2E_DIR/repl.wrong
}

# start NAME DIR ARGS... : run staskd on DIR with the given flags (log in $E2E_DIR/NAME.log)
start() {
  local name=$1 dir=$2; shift 2
  nohup setsid $BIN/staskd -dir $dir "$@" < /dev/null > $E2E_DIR/$name.log 2>&1 &
  E2E_PIDS+=($!)
  LAST_PID=$!
}
alive() { kill -0 $1 2>/dev/null; }
# stop_pid PID [sig]: only a pid in E2E_PIDS
stop_pid() {
  local p=$1 s=${2:-TERM} i
  for q in "${E2E_PIDS[@]}"; do [ "$q" = "$p" ] || continue
    kill -$s $p 2>/dev/null
    for i in $(seq 300); do alive $p || return 0; sleep 0.1; done
    echo "pid $p did not exit"; return 1
  done
}
stop_all() { for p in "${E2E_PIDS[@]}"; do alive $p && stop_pid $p; done; E2E_PIDS=(); }
wait_port() { for i in $(seq 200); do (exec 3<>/dev/tcp/127.0.0.1/$1) 2>/dev/null && return 0; sleep 0.1; done; return 1; }

# psql: the real psql 16 (docker image postgres:16, host network) or a local psql
# usage: PSQLOPTS="PGPASSWORD=.." psql_run PORT [psql args]
psql_run() {
  local port=$1; shift
  if command -v psql > /dev/null; then
    env PGSSLROOTCERT=$CERTS/ca.crt "${PSQLENV[@]}" psql -X -q -w -h 127.0.0.1 -p $port -U app stask "$@"
  else
    local e=(); for kv in "${PSQLENV[@]}"; do e+=(-e "${kv//$CERTS//certs}"); done
    docker run --rm --network host -v $E2E_DIR:/d -v $CERTS:/certs:ro -e PGSSLROOTCERT=/certs/ca.crt "${e[@]}" postgres:16 \
      psql -X -q -w -h 127.0.0.1 -p $port -U app stask "${@//$E2E_DIR//d}"
  fi
}
cleanup_common() { stop_all; }
