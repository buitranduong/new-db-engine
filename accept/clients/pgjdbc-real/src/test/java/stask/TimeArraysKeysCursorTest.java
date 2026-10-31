package stask;

import static org.junit.jupiter.api.Assertions.*;

import java.sql.*;
import java.time.*;
import java.util.*;
import org.junit.jupiter.api.*;

class TimeArraysKeysCursorTest {
    // ---------------------------------------------------------------- timestamps and time zones
    static final String[] ZONES = {"UTC", "Asia/Ho_Chi_Minh", "America/New_York", "Pacific/Kiritimati", "Asia/Kolkata", "Europe/London", "Australia/Lord_Howe"};

    @Test void timestampsRoundTripInEveryJvmZone() throws Exception {
        TimeZone saved = TimeZone.getDefault();
        try {
            for (String z : ZONES) {
                TimeZone.setDefault(TimeZone.getTimeZone(z));
                for (String mode : new String[]{"prepareThreshold=0", "prepareThreshold=1&binaryTransfer=true"}) {
                    try (Connection c = Env.connect(mode)) {
                        String ctx = z + " " + mode;
                        assertEquals(z, Env.str(c, "show timezone").replace("Asia/Saigon", "Asia/Ho_Chi_Minh").replace("Asia/Calcutta", "Asia/Kolkata"), "session TimeZone follows the startup parameter, " + ctx);
                        Env.exec(c, "drop table if exists pj_tz");
                        Env.exec(c, "create table pj_tz(id int primary key, ts timestamp, tstz timestamptz, d date, t time)");
                        Instant[] instants = {Instant.parse("2024-03-10T06:59:59.999999Z"), Instant.parse("2024-03-10T07:00:00Z"), Instant.parse("2024-11-03T05:59:59Z"), Instant.parse("2024-11-03T06:00:00Z"),
                                Instant.parse("1969-07-20T20:17:40Z"), Instant.parse("1900-01-01T00:00:00Z"), Instant.parse("2038-01-19T03:14:08Z"), Instant.parse("2100-12-31T23:59:59.123456Z"), Instant.parse("1700-06-15T12:00:00Z")};
                        for (int round = 0; round < 3; round++) {
                            Env.exec(c, "delete from pj_tz");
                            try (PreparedStatement p = c.prepareStatement("insert into pj_tz(id,ts,tstz,d,t) values (?,?,?,?,?)")) {
                                int id = 0;
                                for (Instant i : instants) {
                                    p.setInt(1, ++id);
                                    p.setObject(2, LocalDateTime.ofInstant(i, ZoneOffset.UTC));
                                    p.setObject(3, i.atOffset(ZoneOffset.UTC));
                                    p.setObject(4, LocalDate.ofInstant(i, ZoneOffset.UTC));
                                    p.setObject(5, LocalTime.ofInstant(i, ZoneOffset.UTC));
                                    p.executeUpdate();
                                }
                            }
                        }
                        try (Statement s = c.createStatement(); ResultSet r = s.executeQuery("select id, ts, tstz, d, t, extract(epoch from tstz)::numeric(20,6)::text, tstz at time zone 'UTC' from pj_tz order by id")) {
                            int id = 0;
                            while (r.next()) {
                                Instant i = instants[id++];
                                assertEquals(LocalDateTime.ofInstant(i, ZoneOffset.UTC), r.getObject(2, LocalDateTime.class), ctx + " ts " + i);
                                assertEquals(i, r.getObject(3, OffsetDateTime.class).toInstant(), ctx + " tstz " + i);
                                assertEquals(i, r.getTimestamp(3).toInstant(), ctx + " getTimestamp " + i);
                                assertEquals(LocalDate.ofInstant(i, ZoneOffset.UTC), r.getObject(4, LocalDate.class), ctx + " date " + i);
                                assertEquals(LocalTime.ofInstant(i, ZoneOffset.UTC), r.getObject(5, LocalTime.class), ctx + " time " + i);
                                // epoch computed by the server agrees with the instant we sent (the server resolved the same absolute time)
                                assertEquals(new java.math.BigDecimal(i.getEpochSecond() + "." + String.format("%06d", i.getNano() / 1000)).setScale(6), new java.math.BigDecimal(r.getString(6)).setScale(6), ctx + " epoch " + i);
                                assertEquals(LocalDateTime.ofInstant(i, ZoneOffset.UTC), r.getObject(7, LocalDateTime.class), ctx + " at time zone UTC " + i);
                            }
                            assertEquals(instants.length, id);
                        }
                        // java.sql.Date / Time / Timestamp with local-zone semantics must not shift by a day
                        try (PreparedStatement p = c.prepareStatement("insert into pj_tz(id,d,ts) values (100,?,?)")) {
                            p.setDate(1, java.sql.Date.valueOf("2024-03-10")); p.setTimestamp(2, Timestamp.valueOf("2024-03-10 02:30:00"));
                            p.executeUpdate();
                        } catch (SQLException e) { fail(ctx + " " + e.getMessage()); }
                        try (Statement s = c.createStatement(); ResultSet r = s.executeQuery("select d::text, ts::text from pj_tz where id=100")) {
                            r.next();
                            assertEquals("2024-03-10", r.getString(1), ctx);
                            // 02:30 does not exist locally in zones that spring forward that night; pgjdbc then sends the shifted wall time
                            LocalDateTime want = Timestamp.valueOf("2024-03-10 02:30:00").toLocalDateTime();
                            assertEquals(want.format(java.time.format.DateTimeFormatter.ofPattern("yyyy-MM-dd HH:mm:ss")), r.getString(2), ctx);
                        }
                        // SET TIME ZONE changes how the server prints and parses timestamptz
                        Env.exec(c, "set time zone 'Asia/Tokyo'");
                        assertEquals("2024-03-10 16:00:00+09", Env.str(c, "select timestamptz '2024-03-10 07:00:00+00'::text"), ctx);
                        assertEquals("2024-03-10 07:00:00+00", Env.str(c, "select (timestamptz '2024-03-10 16:00:00')::timestamptz at time zone 'UTC' || '+00'"), ctx);
                        assertEquals("Asia/Tokyo", Env.str(c, "show timezone"), ctx);
                        Env.exec(c, "set time zone 'America/New_York'");
                        assertEquals("2024-03-10 03:00:00-04", Env.str(c, "select (timestamptz '2024-03-10 07:00:00+00')::text"), ctx);
                        assertEquals("2024-11-03 01:30:00-05", Env.str(c, "select (timestamptz '2024-11-03 06:30:00+00')::text"), ctx);
                        assertEquals("2024-11-03 01:30:00-04", Env.str(c, "select (timestamptz '2024-11-03 05:30:00+00')::text"), ctx);
                        Env.exec(c, "reset time zone");
                        Env.exec(c, "drop table pj_tz");
                    }
                }
            }
        } finally { TimeZone.setDefault(saved); }
    }

    @Test void infinityIntervalsAndLiterals() throws Exception {
        try (Connection c = Env.connect()) {
            try (Statement s = c.createStatement(); ResultSet r = s.executeQuery("select 'infinity'::timestamp, '-infinity'::timestamptz, interval '1 day 02:03:04.5', date '2024-02-29' + 1, timestamp '2024-01-31 10:00' + interval '1 month'")) {
                r.next();
                assertEquals("infinity", r.getString(1)); assertEquals("-infinity", r.getString(2));
                assertEquals("1 day 02:03:04.5", r.getString(3).replace("1 days", "1 day"));
                assertEquals("2024-03-01", r.getString(4)); assertEquals("2024-02-29 10:00:00", r.getString(5));
            }
        }
    }

    // ---------------------------------------------------------------- arrays
    @Test void arraysSqlAndJdbc() throws Exception {
        try (Connection c = Env.connect()) {
            Env.exec(c, "drop table if exists pj_arr"); Env.exec(c, "create table pj_arr(id int primary key, a int[], t text[], m int[][])");
            try (PreparedStatement p = c.prepareStatement("insert into pj_arr values (?,?,?,?)")) {
                p.setInt(1, 1); p.setArray(2, c.createArrayOf("int4", new Integer[]{1, 2, 3})); p.setArray(3, c.createArrayOf("text", new String[]{"x", null, "y z", "{b}"}));
                p.setObject(4, new int[][]{{1, 2}, {3, 4}}); p.executeUpdate();
                p.setInt(1, 2); p.setObject(2, new int[]{}); p.setNull(3, Types.ARRAY); p.setNull(4, Types.ARRAY); p.executeUpdate();
                p.setInt(1, 3); p.setObject(2, new int[]{7, 8}); p.setObject(3, new String[]{"q"}); p.setNull(4, Types.ARRAY); p.executeUpdate();
            }
            try (Statement s = c.createStatement(); ResultSet r = s.executeQuery("select a, t, m, a::text, t::text, m::text, array_length(a,1), cardinality(a), a[2], a || 9, array_append(a, 10), array_to_string(t, ','), 2 = any(a), a @> array[2], a && array[3,99] from pj_arr order by id")) {
                r.next();
                assertArrayEquals(new Integer[]{1, 2, 3}, (Integer[]) r.getArray(1).getArray());
                assertArrayEquals(new String[]{"x", null, "y z", "{b}"}, (String[]) r.getArray(2).getArray());
                assertArrayEquals(new Integer[][]{{1, 2}, {3, 4}}, (Integer[][]) r.getArray(3).getArray());
                assertEquals("{1,2,3}", r.getString(4)); assertEquals("{x,NULL,\"y z\",\"{b}\"}", r.getString(5)); assertEquals("{{1,2},{3,4}}", r.getString(6));
                assertEquals(3, r.getInt(7)); assertEquals(3, r.getInt(8)); assertEquals(2, r.getInt(9)); assertEquals("{1,2,3,9}", r.getString(10)); assertEquals("{1,2,3,10}", r.getString(11));
                assertEquals("x,y z,{b}", r.getString(12)); assertTrue(r.getBoolean(13)); assertTrue(r.getBoolean(14)); assertTrue(r.getBoolean(15));
                r.next(); assertEquals("{}", r.getString(4)); assertNull(r.getArray(2)); assertEquals(0, r.getInt(8));
            }
            assertEquals("{1,2,3}", Env.str(c, "select array_agg(g order by g)::text from generate_series(1,3) g"));
            assertEquals(6, Env.scalar(c, "select sum(x) from pj_arr, unnest(a) x where id=1"));
            assertEquals("{{1,2},{3,4}}", Env.str(c, "select m::text from pj_arr where id=1"));
            assertEquals(1, Env.scalar(c, "select count(*) from pj_arr where a = '{7,8}'"));
            try (PreparedStatement p = c.prepareStatement("select id from pj_arr where id = any(?) order by id")) {
                p.setArray(1, c.createArrayOf("int4", new Integer[]{1, 3, 99}));
                try (ResultSet r = p.executeQuery()) { assertTrue(r.next()); assertEquals(1, r.getInt(1)); assertTrue(r.next()); assertEquals(3, r.getInt(1)); assertFalse(r.next()); }
            }
            Env.exec(c, "drop table pj_arr");
        }
    }

    // ---------------------------------------------------------------- generated keys
    @Test void generatedKeys() throws Exception {
        for (String mode : new String[]{"prepareThreshold=0", "prepareThreshold=1&binaryTransfer=true"}) {
            try (Connection c = Env.connect(mode)) {
                Env.exec(c, "drop table if exists pj_gk");
                Env.exec(c, "create table pj_gk(id bigint generated by default as identity primary key, sid serial, u uuid default gen_random_uuid(), name text, n int default 5)");
                for (int round = 0; round < 3; round++) {
                    try (PreparedStatement p = c.prepareStatement("insert into pj_gk(name) values (?)", Statement.RETURN_GENERATED_KEYS)) {
                        p.setString(1, "a" + round); assertEquals(1, p.executeUpdate());
                        try (ResultSet k = p.getGeneratedKeys()) { assertTrue(k.next()); assertTrue(k.getLong("id") > 0, mode); assertTrue(k.getInt("sid") > 0); assertNotNull(k.getObject("u")); assertEquals(5, k.getInt("n")); assertEquals("a" + round, k.getString("name")); assertFalse(k.next()); }
                    }
                }
                try (PreparedStatement p = c.prepareStatement("insert into pj_gk(name) values (?)", new String[]{"id", "u"})) {
                    p.setString(1, "cols"); p.executeUpdate();
                    try (ResultSet k = p.getGeneratedKeys()) { assertEquals(2, k.getMetaData().getColumnCount()); assertTrue(k.next()); assertTrue(k.getLong(1) > 3); assertNotNull(k.getObject(2)); }
                }
                // batch + generated keys
                try (PreparedStatement p = c.prepareStatement("insert into pj_gk(name) values (?)", Statement.RETURN_GENERATED_KEYS)) {
                    for (int i = 0; i < 25; i++) { p.setString(1, "b" + i); p.addBatch(); }
                    int[] cnt = p.executeBatch(); assertEquals(25, cnt.length);
                    for (int x : cnt) assertTrue(x == 1 || x == Statement.SUCCESS_NO_INFO, "batch count " + x);
                    Set<Long> ids = new TreeSet<>(); try (ResultSet k = p.getGeneratedKeys()) { while (k.next()) ids.add(k.getLong("id")); }
                    assertEquals(25, ids.size(), "distinct generated keys from a batch");
                }
                try (Statement s = c.createStatement()) {
                    assertEquals(1, s.executeUpdate("insert into pj_gk(name) values ('stmt')", Statement.RETURN_GENERATED_KEYS));
                    try (ResultSet k = s.getGeneratedKeys()) { assertTrue(k.next()); assertTrue(k.getLong(1) > 0); }
                }
                // ON CONFLICT .. RETURNING, DML RETURNING with expressions
                try (Statement s = c.createStatement(); ResultSet r = s.executeQuery("insert into pj_gk(id,name) values (1000,'oc') on conflict (id) do update set name = excluded.name || '!' returning id, name")) { r.next(); assertEquals("oc", r.getString(2)); }
                try (Statement s = c.createStatement(); ResultSet r = s.executeQuery("insert into pj_gk(id,name) values (1000,'oc') on conflict (id) do update set name = excluded.name || '!' returning id, name")) { r.next(); assertEquals("oc!", r.getString(2)); }
                assertEquals(1, Env.scalar(c, "select count(*) from pj_gk where id=1000"));
                // identity continues after explicit id insert (no duplicate-key from the sequence)
                try (PreparedStatement p = c.prepareStatement("insert into pj_gk(name) values ('after')", Statement.RETURN_GENERATED_KEYS)) { p.executeUpdate(); try (ResultSet k = p.getGeneratedKeys()) { k.next(); assertNotEquals(1000L, k.getLong(1)); } }
                Env.exec(c, "drop table pj_gk");
            }
        }
    }

    // ---------------------------------------------------------------- cursors / fetchSize
    @Test void largeResultSetsWithFetchSizeCursors() throws Exception {
        final int N = 300_000;
        try (Connection c = Env.connect()) {
            Env.exec(c, "drop table if exists pj_big");
            Env.exec(c, "create table pj_big(id int primary key, grp int, s text)");
            long n = ((org.postgresql.PGConnection) c).getCopyAPI().copyIn("copy pj_big from stdin", new java.io.Reader() {
                int i = 0; String cur = ""; int pos = 0;
                public int read(char[] b, int off, int len) {
                    int w = 0;
                    while (w < len) {
                        if (pos >= cur.length()) { if (i >= N) break; i++; cur = i + "\t" + (i % 100) + "\tvalue-" + i + "\n"; pos = 0; }
                        b[off + w++] = cur.charAt(pos++);
                    }
                    return w == 0 ? -1 : w;
                }
                public void close() { }
            });
            assertEquals(N, n);
            long expectSum = (long) N * (N + 1) / 2;
            for (String mode : new String[]{"", "prepareThreshold=1&binaryTransfer=true"}) {
                try (Connection d = Env.connect(mode)) {
                    d.setAutoCommit(false);
                    try (Statement s = d.createStatement()) {
                        s.setFetchSize(1000);
                        long cnt = 0, sum = 0;
                        try (ResultSet r = s.executeQuery("select id, s from pj_big order by id")) {
                            long prev = 0; while (r.next()) { long id = r.getInt(1); assertEquals(prev + 1, id); prev = id; cnt++; sum += id; }
                        }
                        assertEquals(N, cnt, mode); assertEquals(expectSum, sum);
                        // hash aggregation behind a cursor
                        s.setFetchSize(7); cnt = 0;
                        try (ResultSet r = s.executeQuery("select grp, count(*) from pj_big group by grp order by grp")) { while (r.next()) { cnt++; assertEquals(N / 100, r.getInt(2)); } }
                        assertEquals(100, cnt);
                    }
                    // two cursors open at once, interleaved, on one connection
                    try (PreparedStatement a = d.prepareStatement("select id from pj_big order by id", ResultSet.TYPE_FORWARD_ONLY, ResultSet.CONCUR_READ_ONLY);
                         PreparedStatement b = d.prepareStatement("select id from pj_big order by id desc")) {
                        a.setFetchSize(500); b.setFetchSize(333);
                        try (ResultSet ra = a.executeQuery(); ResultSet rb = b.executeQuery()) {
                            for (int i = 1; i <= 5000; i++) { assertTrue(ra.next()); assertTrue(rb.next()); assertEquals(i, ra.getInt(1)); assertEquals(N - i + 1, rb.getInt(1)); }
                        }
                    }
                    // early close, then reuse the connection
                    try (Statement s = d.createStatement()) { s.setFetchSize(100); try (ResultSet r = s.executeQuery("select * from pj_big")) { r.next(); r.next(); } }
                    assertEquals(N, Env.scalar(d, "select count(*) from pj_big"));
                    // maxRows with fetchSize
                    try (Statement s = d.createStatement()) { s.setFetchSize(10); s.setMaxRows(25); int k = 0; try (ResultSet r = s.executeQuery("select id from pj_big order by id")) { while (r.next()) k++; } assertEquals(25, k); }
                    d.commit();
                }
            }
            // autocommit on: fetchSize is ignored by the driver (everything fetched), result must still be complete
            try (Statement s = c.createStatement()) { s.setFetchSize(100); long k = 0; try (ResultSet r = s.executeQuery("select id from pj_big")) { while (r.next()) k++; } assertEquals(N, k); }
            // defaultRowFetchSize URL parameter
            try (Connection d = Env.connect("defaultRowFetchSize=2000")) {
                d.setAutoCommit(false);
                try (Statement s = d.createStatement(); ResultSet r = s.executeQuery("select id from pj_big where id % 3 = 0")) { long k = 0; while (r.next()) k++; assertEquals(N / 3, k); }
                // update in the same transaction while a cursor is open on another statement
                try (Statement s1 = d.createStatement(); Statement s2 = d.createStatement()) {
                    s1.setFetchSize(100);
                    try (ResultSet r = s1.executeQuery("select id from pj_big where id <= 1000 order by id")) {
                        int k = 0; while (r.next()) { k++; if (k == 10) assertEquals(1, s2.executeUpdate("update pj_big set grp = -1 where id = 5000")); }
                        assertEquals(1000, k);
                    }
                }
                d.rollback();
            }
            assertEquals(0, Env.scalar(c, "select count(*) from pj_big where grp = -1"));
            Env.exec(c, "drop table pj_big");
        }
    }
}
