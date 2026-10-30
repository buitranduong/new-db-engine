. /tmp/crm-run/env.sh
[ -f /tmp/crm-run/stask.pid ] && kill $(cat /tmp/crm-run/stask.pid) 2>/dev/null; sleep 1
rm -rf /tmp/crm-run/db
nohup setsid /tmp/crm-bin/staskd -dir /tmp/crm-run/db -addr 127.0.0.1:28603 -pg-addr 127.0.0.1:28602 -pg-password "$CRM_DB_PASSWORD" -pg-auth scram -slow-query 1ns </dev/null >/tmp/crm-run/staskd.log 2>&1 &
echo $! > /tmp/crm-run/stask.pid; sleep 2
