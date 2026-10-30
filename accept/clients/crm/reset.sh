. /tmp/crm-run/env.sh
export PGPASSWORD=$CRM_DB_PASSWORD
for port in 28601 28602; do
  docker run --rm -i --network host -e PGPASSWORD postgres:16 env PGSSLMODE=disable psql -X -h 127.0.0.1 -p $port -U app crm -q <<'SQL'
delete from branch; delete from warehouse; delete from seat_layout; delete from route; delete from vehicle_owner; delete from business_type;
delete from master_data where name like 'Đối tác%' and parent_id is not null;
SQL
done
