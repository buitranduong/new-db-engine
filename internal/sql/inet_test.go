package sql

import "testing"

func TestInetCidrMacaddr(t *testing.T) {
	s := NewSession(newEngine(t))
	one := func(q string) string {
		t.Helper()
		return run(t, s, q).Rows[0][0].String()
	}
	for q, want := range map[string]string{
		`SELECT '192.168.1.5'::inet`:                           "192.168.1.5",
		`SELECT '192.168.1.5/32'::inet`:                        "192.168.1.5",
		`SELECT '192.168.1.5/24'::inet`:                        "192.168.1.5/24",
		`SELECT '192.168.1.0/24'::cidr`:                        "192.168.1.0/24",
		`SELECT '10.1.2.3'::cidr`:                              "10.1.2.3/32",
		`SELECT '::1'::inet`:                                   "::1",
		`SELECT '2001:DB8:0:0:0:0:0:1'::inet`:                  "2001:db8::1",
		`SELECT host('192.168.1.5/24'::inet)`:                  "192.168.1.5",
		`SELECT family('192.168.1.5'::inet)`:                   "4",
		`SELECT family('::1'::inet)`:                           "6",
		`SELECT masklen('192.168.1.5/24'::inet)`:               "24",
		`SELECT netmask('192.168.1.5/24'::inet)`:               "255.255.255.0",
		`SELECT network('192.168.1.5/24'::inet)`:               "192.168.1.0/24",
		`SELECT '192.168.1.5'::inet << '192.168.1.0/24'::cidr`: "t",
		`SELECT '192.168.2.5'::inet << '192.168.1.0/24'::cidr`: "f",
		`SELECT '192.168.1.0/24'::cidr >> '192.168.1.5'::inet`: "t",
		`SELECT '08:00:2B:01:02:03'::macaddr`:                  "08:00:2b:01:02:03",
		`SELECT '08002b010203'::macaddr`:                       "08:00:2b:01:02:03",
		`SELECT pg_typeof('1.2.3.4'::inet)::text`:              "inet",
		`SELECT '1.2.3.4'::inet = '1.2.3.4/32'::inet`:          "t",
		`SELECT 1 << 3`:                                        "8",
	} {
		if got := one(q); got != want {
			t.Errorf("%s = %q want %q", q, got, want)
		}
	}
	for _, q := range []string{`SELECT '192.168.1.5/24'::cidr`, `SELECT 'nope'::inet`, `SELECT '1.2.3'::inet`, `SELECT 'zz:zz'::macaddr`} {
		if _, err := tryRun(s, q); err == nil || AsError(err).Code != "22P02" {
			t.Errorf("%s: %v", q, err)
		}
	}
	run(t, s, `CREATE TABLE hosts (id BIGINT PRIMARY KEY, ip inet NOT NULL, net cidr, mac macaddr)`)
	run(t, s, `INSERT INTO hosts VALUES (1, '10.0.0.1', '10.0.0.0/8', '08:00:2b:01:02:03'), (2, '10.0.0.2/24', NULL, NULL)`)
	run(t, s, `CREATE UNIQUE INDEX hosts_ip ON hosts (ip)`)
	if _, err := tryRun(s, `INSERT INTO hosts VALUES (3, '10.0.0.1/32', NULL, NULL)`); err == nil {
		t.Error("equal inet values (one written with /32) not unique")
	}
	if got := ids(t, s, `SELECT id FROM hosts WHERE ip = '10.0.0.1'`); got != "1" {
		t.Errorf("lookup: %q", got)
	}
	if got := ids(t, s, `SELECT id FROM hosts WHERE ip << '10.0.0.0/8'::cidr ORDER BY id`); got != "1,2" {
		t.Errorf("containment filter: %q", got)
	}
	r := run(t, s, `SELECT ip, net, mac FROM hosts WHERE id = 2`)
	if r.Rows[0][0].String() != "10.0.0.2/24" {
		t.Errorf("stored: %v", r.Rows[0])
	}
	if _, err := tryRun(s, `INSERT INTO hosts VALUES (4, 'bogus', NULL, NULL)`); err == nil {
		t.Error("invalid inet stored")
	}
	if got := run(t, s, `SELECT format_type(atttypid, atttypmod) FROM pg_attribute WHERE attrelid = 'hosts'::regclass AND attname = 'ip'`).Rows[0][0].String(); got != "inet" {
		t.Errorf("format_type %q", got)
	}
}
