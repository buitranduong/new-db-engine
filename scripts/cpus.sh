# Sourced by the benchmark scripts: the server (STASK or PostgreSQL) and the
# client run on separate cores, the same for both systems, so that neither
# steals the other's CPU (docs/11 §5). SERVER_CPUS / CLIENT_CPUS override
# the defaults; set one to the empty string to leave that side unpinned.
SERVER_CPUS=${SERVER_CPUS-0-5}
CLIENT_CPUS=${CLIENT_CPUS-6-7}
SPIN=""; [ -n "$SERVER_CPUS" ] && SPIN="taskset -c $SERVER_CPUS"
CPIN=""; [ -n "$CLIENT_CPUS" ] && CPIN="taskset -c $CLIENT_CPUS"
# pgpin pins the PostgreSQL container started by bench/pg/pg.sh
pgpin() {
  [ -n "$SERVER_CPUS" ] && sudo docker update --cpuset-cpus "$SERVER_CPUS" stask-pg-baseline > /dev/null
  return 0
}
