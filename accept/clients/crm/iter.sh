# rebuild staskd, fresh db, boot the CRM app against it, wait for first ERROR/Started
export PATH=$PATH:/usr/local/go/bin
true
bash /tmp/crm-run/start-stask.sh
pkill -f 'com.dth.crm.CRMApplication' 2>/dev/null
CL=${CLX:-changelog-crmcompat.xml} bash /tmp/crm-run/boot.sh stask
for i in $(seq 1 100); do
  sleep 5
  if grep -aqE ' ERROR |Started CRM|BUILD FAILED' /tmp/crm-run/app-stask.log; then break; fi
done
grep -anE ' ERROR |Started CRM|BUILD FAILED' /tmp/crm-run/app-stask.log | cut -c1-700 | head -5
