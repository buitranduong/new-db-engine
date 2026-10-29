#!/bin/bash
# Connection-scale stress test over the PostgreSQL protocol.
# usage: scripts/stress.sh 1000 10000 100000 1000000
# Raises kernel limits for the run (sysctl, not persisted).
. ~/.stask_env
cd ~/stask
. scripts/cpus.sh
set -u
sudo sysctl -q -w net.core.somaxconn=65535 net.ipv4.tcp_max_syn_backlog=65535 \
  net.ipv4.ip_local_port_range="1024 65535" fs.nr_open=2200000 fs.file-max=4000000 \
  net.netfilter.nf_conntrack_max=2500000 net.core.netdev_max_backlog=65535
sudo prlimit --pid $$ --nofile=2100000:2100000
ulimit -n
R=${RES:-results/stress}; mkdir -p $R
for n in "$@"; do
  pkill -x staskd; sleep 1
  rm -rf $DATA/stress
  $SPIN ./bin/staskd -dir $DATA/stress -addr 127.0.0.1:17072 -pg-addr 127.0.0.1:17432 -segment-mb 256 > $R/server-$n.log 2>&1 &
  spid=$!
  sleep 1
  dur=30s
  [ $n -ge 100000 ] && dur=60s
  python3 scripts/sysmon.py 1800 > $R/sysmon-$n.csv 2>/dev/null &
  mpid=$!
  $CPIN ./bin/staskstress -conns $n -src 127.0.0.1-127.0.0.40 -duration $dur -server-pid $spid -label conns-$n \
    > $R/conns-$n.json 2> $R/conns-$n.log
  echo "exit=$? conns=$n" >> $R/conns-$n.log
  kill $mpid 2>/dev/null
  grep -iE "panic|fatal|too many|error" $R/server-$n.log | sort | uniq -c | head -5 > $R/server-$n.errors
  kill $spid; wait $spid 2>/dev/null
  echo "done $n"
done
echo STRESSDONE
