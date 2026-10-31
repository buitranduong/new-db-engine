package stask;

import static org.junit.jupiter.api.Assertions.*;

import java.math.BigDecimal;
import java.sql.*;
import java.time.*;
import java.util.*;
import org.junit.jupiter.api.*;
import org.postgresql.PGStatement;

/** Server-side prepared statements (statement cache) and binary transfer, as Hibernate/EclipseLink/Hikari drive them. */
class PreparedAndBinaryTest {
    @Test
    void serverPreparedStatementsAndCacheSurviveDdl() throws Exception {
        try (Connection c = Env.connect("prepareThreshold=1&preparedStatementCacheQueries=64&preparedStatementCacheSizeMiB=4")) {
            Env.exec(c, "drop table if exists pj_ps");
            Env.exec(c, "create table pj_ps(id bigint primary key, n int, s text)");
            try (PreparedStatement ins = c.prepareStatement("insert into pj_ps(id,n,s) values (?,?,?)");
                 PreparedStatement sel = c.prepareStatement("select id,n,s from pj_ps where n >= ? order by id")) {
                for (int i = 0; i < 20; i++) {
                    ins.setLong(1, i); ins.setInt(2, i * 10); ins.setString(3, "v" + i); assertEquals(1, ins.executeUpdate());
                    sel.setInt(1, 50);
                    int n = 0; try (ResultSet r = sel.executeQuery()) { while (r.next()) n++; }
                    assertEquals(Math.max(0, i - 4), n, "iteration " + i);
                }
                assertTrue(((PGStatement) sel).isUseServerPrepare(), "statement became server prepared after threshold");
                // the same SQL text again on a second statement object reuses the cached server statement
                try (PreparedStatement sel2 = c.prepareStatement("select id,n,s from pj_ps where n >= ? order by id")) {
                    sel2.setInt(1, 0); int n = 0; try (ResultSet r = sel2.executeQuery()) { while (r.next()) n++; } assertEquals(20, n);
                }
                // DDL that changes the result type of a cached "select id,n,s": must not return stale shape or fail permanently
                Env.exec(c, "alter table pj_ps alter column n type bigint");
                sel.setInt(1, 0);
                try (ResultSet r = sel.executeQuery()) { assertEquals(Types.BIGINT, r.getMetaData().getColumnType(2)); int n = 0; while (r.next()) n++; assertEquals(20, n); }
                Env.exec(c, "alter table pj_ps add column extra text default 'x'");
                try (PreparedStatement all = c.prepareStatement("select * from pj_ps where id = ?")) {
                    for (int i = 0; i < 4; i++) { all.setLong(1, 1); try (ResultSet r = all.executeQuery()) { assertTrue(r.next()); assertEquals(4, r.getMetaData().getColumnCount()); } }
                    Env.exec(c, "alter table pj_ps add column extra2 int");
                    all.setLong(1, 1);
                    try (ResultSet r = all.executeQuery()) { assertTrue(r.next()); assertEquals(5, r.getMetaData().getColumnCount(), "select * sees the new column after ALTER (cached plan invalidated)"); }
                }
                // drop + recreate under the same name with a different shape
                Env.exec(c, "drop table pj_ps");
                Env.exec(c, "create table pj_ps(id bigint primary key, n int, s text)");
                sel.setInt(1, 0);
                try (ResultSet r = sel.executeQuery()) { assertFalse(r.next()); }
            }
            Env.exec(c, "drop table pj_ps");
        }
    }

    @Test
    void manyDistinctStatementsEvictFromCache() throws Exception {
        try (Connection c = Env.connect("prepareThreshold=1&preparedStatementCacheQueries=8")) {
            for (int i = 0; i < 100; i++) {
                try (PreparedStatement p = c.prepareStatement("select ?::int + " + i)) {
                    for (int k = 0; k < 3; k++) { p.setInt(1, 1); try (ResultSet r = p.executeQuery()) { assertTrue(r.next()); assertEquals(1 + i, r.getInt(1)); } }
                }
            }
            long open = Env.scalar(c, "select count(*) from pg_prepared_statements");
            assertTrue(open <= 100, "server-side prepared statements bounded by the driver cache, got " + open);
        }
    }

    /** Values round-trip identically in text mode and in binary mode (prepareThreshold=1 + binaryTransfer). */
    @Test
    void binaryTransferRoundTripAllTypes() throws Exception {
        String ddl = "create table pj_bin(id int primary key, i2 smallint, i4 int, i8 bigint, f4 real, f8 double precision, num numeric(30,10), b boolean,"
                + " bt bytea, d date, t time, ts timestamp, tstz timestamptz, u uuid, txt text, vc varchar(40),"
                + " a4 int[], a8 bigint[], at text[], af float8[], ab boolean[], an numeric[], ats timestamp[], au uuid[])";
        Object[][] data = {
                {1, (short) 7, 123456, 9_000_000_000_000L, 1.5f, 2.718281828459045, new BigDecimal("12345678901234567890.1234567890"), true,
                        new byte[]{0, 1, 2, (byte) 255, (byte) 128}, LocalDate.of(2024, 2, 29), LocalTime.of(13, 45, 59, 123_456_000), LocalDateTime.of(2024, 3, 10, 2, 30, 0, 999_999_000),
                        OffsetDateTime.of(2024, 11, 3, 1, 30, 0, 5_000, ZoneOffset.ofHours(-4)), UUID.fromString("123e4567-e89b-12d3-a456-426614174000"), "xin chào Đỗ ☃", "v",
                        new Integer[]{1, null, 3}, new Long[]{Long.MAX_VALUE, Long.MIN_VALUE}, new String[]{"a,b", "q\"uote", null, "", "\\back", "é"}, new Double[]{1.0, -0.0, 1e300},
                        new Boolean[]{true, null, false}, new BigDecimal[]{new BigDecimal("1.50"), new BigDecimal("-0.0001"), new BigDecimal("1E+3")},
                        new Timestamp[]{Timestamp.valueOf("2020-01-01 00:00:00.000001")}, new UUID[]{UUID.fromString("00000000-0000-0000-0000-000000000001")}},
                {2, (short) -32768, Integer.MIN_VALUE, Long.MIN_VALUE, -0.0f, Double.NaN, new BigDecimal("-0.0000000001"), false,
                        new byte[0], LocalDate.of(1900, 1, 1), LocalTime.MIDNIGHT, LocalDateTime.of(1899, 12, 31, 23, 59, 59), OffsetDateTime.of(1969, 12, 31, 23, 59, 59, 0, ZoneOffset.UTC),
                        UUID.fromString("ffffffff-ffff-ffff-ffff-ffffffffffff"), "", "",
                        new Integer[0], new Long[0], new String[0], new Double[]{Double.POSITIVE_INFINITY}, new Boolean[0], new BigDecimal[0], new Timestamp[0], new UUID[0]},
        };
        Map<String, List<Object>> got = new LinkedHashMap<>();
        for (String params : new String[]{"prepareThreshold=0", "prepareThreshold=1&binaryTransfer=true", "prepareThreshold=-1&binaryTransfer=true", "prepareThreshold=1&binaryTransfer=false"}) {
            try (Connection c = Env.connect(params)) {
                Env.exec(c, "drop table if exists pj_bin"); Env.exec(c, ddl);
                for (int round = 0; round < 3; round++) { // crosses the server-prepare threshold
                    Env.exec(c, "delete from pj_bin");
                    try (PreparedStatement p = c.prepareStatement("insert into pj_bin values (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")) {
                        for (Object[] row : data) {
                            for (int k = 0; k < row.length; k++) {
                                Object v = row[k];
                                if (v instanceof Integer[] x) p.setArray(k + 1, c.createArrayOf("int4", x));
                                else if (v instanceof Long[] x) p.setArray(k + 1, c.createArrayOf("int8", x));
                                else if (v instanceof String[] x) p.setArray(k + 1, c.createArrayOf("text", x));
                                else if (v instanceof Double[] x) p.setArray(k + 1, c.createArrayOf("float8", x));
                                else if (v instanceof Boolean[] x) p.setArray(k + 1, c.createArrayOf("bool", x));
                                else if (v instanceof BigDecimal[] x) p.setArray(k + 1, c.createArrayOf("numeric", x));
                                else if (v instanceof Timestamp[] x) p.setArray(k + 1, c.createArrayOf("timestamp", x));
                                else if (v instanceof UUID[] x) p.setArray(k + 1, c.createArrayOf("uuid", x));
                                else if (v instanceof byte[] x) p.setBytes(k + 1, x);
                                else if (v instanceof Short x) p.setShort(k + 1, x);
                                else p.setObject(k + 1, v);
                            }
                            assertEquals(1, p.executeUpdate());
                        }
                    }
                }
                List<Object> rows = new ArrayList<>();
                try (PreparedStatement q = c.prepareStatement("select * from pj_bin order by id")) {
                    for (int round = 0; round < 3; round++) {
                        rows.clear();
                        try (ResultSet r = q.executeQuery()) {
                            int cols = r.getMetaData().getColumnCount();
                            while (r.next()) {
                                List<Object> row = new ArrayList<>();
                                for (int k = 1; k <= cols; k++) {
                                    Object o = r.getObject(k);
                                    if (o instanceof java.sql.Array a) o = Arrays.deepToString((Object[]) a.getArray());
                                    else if (o instanceof byte[] b) o = Arrays.toString(b);
                                    else if (o instanceof Timestamp t) o = t.toInstant().toString();
                                    row.add(o);
                                }
                                rows.add(row);
                            }
                        }
                    }
                }
                got.put(params, new ArrayList<>(rows));
                // typed accessors on row 1
                try (Statement s = c.createStatement(); ResultSet r = s.executeQuery("select * from pj_bin where id=1")) {
                    r.next();
                    assertEquals(new BigDecimal("12345678901234567890.1234567890"), r.getBigDecimal("num"), params);
                    assertEquals(LocalDate.of(2024, 2, 29), r.getObject("d", LocalDate.class), params);
                    assertEquals(LocalTime.of(13, 45, 59, 123_456_000), r.getObject("t", LocalTime.class), params);
                    assertEquals(LocalDateTime.of(2024, 3, 10, 2, 30, 0, 999_999_000), r.getObject("ts", LocalDateTime.class), params);
                    assertEquals(OffsetDateTime.of(2024, 11, 3, 1, 30, 0, 5_000, ZoneOffset.ofHours(-4)).toInstant(), r.getObject("tstz", OffsetDateTime.class).toInstant(), params);
                    assertEquals(UUID.fromString("123e4567-e89b-12d3-a456-426614174000"), r.getObject("u", UUID.class), params);
                    assertArrayEquals(new byte[]{0, 1, 2, (byte) 255, (byte) 128}, r.getBytes("bt"), params);
                    assertEquals("xin chào Đỗ ☃", r.getString("txt"), params);
                }
                try (Statement s = c.createStatement(); ResultSet r = s.executeQuery("select f8, f4 from pj_bin where id=2")) {
                    r.next(); assertTrue(Double.isNaN(r.getDouble(1)), params); assertEquals(-0.0f, r.getFloat(2), params);
                }
                Env.exec(c, "drop table pj_bin");
            }
        }
        List<Object> ref = got.values().iterator().next();
        for (var e : got.entrySet()) assertEquals(ref.toString(), e.getValue().toString(), "result differs between transfer modes: " + e.getKey());
    }

    @Test
    void parameterTypeInferenceAndNullsInBinary() throws Exception {
        try (Connection c = Env.connect("prepareThreshold=1&binaryTransfer=true")) {
            for (int round = 0; round < 4; round++) {
                try (PreparedStatement p = c.prepareStatement("select ? is null, coalesce(?::int, -1), ?::text || 'x', (?::numeric + 1)::text, ?::timestamptz is null, ?::uuid is null")) {
                    p.setNull(1, Types.VARCHAR); p.setNull(2, Types.INTEGER); p.setString(3, "a"); p.setBigDecimal(4, new BigDecimal("1.25")); p.setNull(5, Types.TIMESTAMP_WITH_TIMEZONE); p.setNull(6, Types.OTHER);
                    try (ResultSet r = p.executeQuery()) { assertTrue(r.next()); assertTrue(r.getBoolean(1)); assertEquals(-1, r.getInt(2)); assertEquals("ax", r.getString(3)); assertEquals("2.25", r.getString(4)); assertTrue(r.getBoolean(5)); assertTrue(r.getBoolean(6)); }
                }
                try (PreparedStatement p = c.prepareStatement("select count(*) from generate_series(1,100) g where g = any(?) and g <> all(?)")) {
                    p.setArray(1, c.createArrayOf("int4", new Integer[]{1, 2, 3, 50})); p.setArray(2, c.createArrayOf("int4", new Integer[]{2}));
                    try (ResultSet r = p.executeQuery()) { r.next(); assertEquals(3, r.getInt(1)); }
                }
            }
        }
    }
}
