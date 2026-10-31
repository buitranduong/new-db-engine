package stask;

import static org.junit.jupiter.api.Assertions.*;

import java.io.*;
import java.nio.charset.StandardCharsets;
import java.sql.*;
import java.util.*;
import org.junit.jupiter.api.*;
import org.postgresql.PGConnection;
import org.postgresql.copy.*;

/** CopyManager COPY IN / OUT, text, CSV and binary, errors, cancellation and large streams. */
class CopyTest {
    static Connection c;
    @BeforeAll static void up() throws Exception { c = Env.connect(); }
    @AfterAll static void down() throws Exception { Env.exec(c, "drop table if exists pj_cp"); Env.exec(c, "drop table if exists pj_cp2"); c.close(); }
    @BeforeEach void fresh() throws Exception {
        Env.exec(c, "drop table if exists pj_cp"); Env.exec(c, "drop table if exists pj_cp2");
        Env.exec(c, "create table pj_cp(id bigint primary key, name text, score double precision, ok boolean, ts timestamp, bin bytea, u uuid, n int)");
        Env.exec(c, "create table pj_cp2(id bigint primary key, name text, score double precision, ok boolean, ts timestamp, bin bytea, u uuid, n int)");
    }
    CopyManager cm() throws SQLException { return ((PGConnection) c).getCopyAPI(); }

    @Test void textCopyInEscapesAndNulls() throws Exception {
        String data = "1\ttab\\there\t1.5\tt\t2024-01-02 03:04:05.123456\t\\\\x00ff\t123e4567-e89b-12d3-a456-426614174000\t\\N\n"
                + "2\tnew\\nline\t-2\tf\t\\N\t\\\\xdeadbeef\t\\N\t7\n"
                + "3\tback\\\\slash Đỗ\t0\tt\t1999-12-31 23:59:59\t\\\\x\t\\N\t0\n";
        long n = cm().copyIn("copy pj_cp from stdin", new StringReader(data));
        assertEquals(3, n);
        try (Statement s = c.createStatement(); ResultSet r = s.executeQuery("select id,name,score,ok,ts::text,encode(bin,'hex'),u::text,n from pj_cp order by id")) {
            assertTrue(r.next()); assertEquals("tab\there", r.getString(2)); assertEquals(1.5, r.getDouble(3)); assertTrue(r.getBoolean(4)); assertEquals("2024-01-02 03:04:05.123456", r.getString(5)); assertEquals("00ff", r.getString(6)); assertNull(r.getObject(8));
            assertTrue(r.next()); assertEquals("new\nline", r.getString(2)); assertNull(r.getString(5)); assertEquals(7, r.getInt(8));
            assertTrue(r.next()); assertEquals("back\\slash Đỗ", r.getString(2));
        }
        assertEquals(3, Env.scalar(c, "select count(*) from pj_cp"));
    }

    @Test void csvCopyWithHeaderQuotesAndDelimiter() throws Exception {
        String csv = "id|name|score|ok|ts|bin|u|n\n1|\"a|b\"|1.25|true|2024-05-05 10:00:00||| 5\n2|\"q\"\"uote\"|2|false|||| \n3|\"multi\nline\"|3|true|2000-01-01 00:00:00|||\n";
        // columns with empty strings: NULL in CSV; spaces -> numeric parse of ' 5' is allowed
        long n = cm().copyIn("copy pj_cp (id,name,score,ok,ts,bin,u,n) from stdin with (format csv, header true, delimiter '|', null '')", new StringReader(csv.replace("||| 5", "|||5").replace("|||| \n", "||||\n")));
        assertEquals(3, n);
        assertEquals("a|b", Env.str(c, "select name from pj_cp where id=1"));
        assertEquals("q\"uote", Env.str(c, "select name from pj_cp where id=2"));
        assertEquals("multi\nline", Env.str(c, "select name from pj_cp where id=3"));
        assertEquals(5, Env.scalar(c, "select n from pj_cp where id=1"));
        StringWriter w = new StringWriter();
        assertEquals(3, cm().copyOut("copy (select id,name from pj_cp order by id) to stdout with (format csv, header true)", w));
        assertEquals("id,name\n1,a|b\n2,\"q\"\"uote\"\n3,\"multi\nline\"\n", w.toString().replace("\r\n", "\n"));
    }

    /** Hand-built PGCOPY binary stream: int8, text, float8, bool, timestamp, bytea, uuid, int4 with NULLs. */
    static byte[] binary(int rows) throws IOException {
        ByteArrayOutputStream bo = new ByteArrayOutputStream(); DataOutputStream o = new DataOutputStream(bo);
        o.write(new byte[]{'P', 'G', 'C', 'O', 'P', 'Y', '\n', (byte) 0xff, '\r', '\n', 0}); o.writeInt(0); o.writeInt(0);
        for (int i = 1; i <= rows; i++) {
            o.writeShort(8);
            o.writeInt(8); o.writeLong(i);
            byte[] t = ("row " + i + " Đ").getBytes(StandardCharsets.UTF_8); o.writeInt(t.length); o.write(t);
            o.writeInt(8); o.writeDouble(i / 4.0);
            o.writeInt(1); o.writeByte(i % 2);
            o.writeInt(8); o.writeLong((i * 1_000_000L) - 30L * 365 * 86400 * 1_000_000L); // microseconds since 2000-01-01
            if (i % 3 == 0) o.writeInt(-1); else { o.writeInt(3); o.write(new byte[]{(byte) i, 0, (byte) 255}); }
            o.writeInt(16); o.writeLong(0x123e4567e89b12d3L); o.writeLong(i);
            if (i % 5 == 0) o.writeInt(-1); else { o.writeInt(4); o.writeInt(-i); }
        }
        o.writeShort(-1);
        return bo.toByteArray();
    }

    @Test void binaryCopyInAndOut() throws Exception {
        long n = cm().copyIn("copy pj_cp from stdin with (format binary)", new ByteArrayInputStream(binary(1000)));
        assertEquals(1000, n);
        assertEquals(1000, Env.scalar(c, "select count(*) from pj_cp"));
        assertEquals("row 7 Đ", Env.str(c, "select name from pj_cp where id=7"));
        assertEquals(1.75, Env.scalar(c, "select score*4 from pj_cp where id=7") / 4.0);
        assertEquals(200, Env.scalar(c, "select count(*) from pj_cp where n is null"));
        assertEquals(333, Env.scalar(c, "select count(*) from pj_cp where bin is null"));
        assertEquals("0700ff", Env.str(c, "select encode(bin,'hex') from pj_cp where id=7"));
        assertEquals("123e4567-e89b-12d3-0000-000000000007", Env.str(c, "select u::text from pj_cp where id=7"));
        assertEquals(7, Env.scalar(c, "select extract(epoch from ts - timestamp '2000-01-01')::bigint + 30*365*86400 from pj_cp where id=7"));
        // binary out then binary in again: identical contents
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        assertEquals(1000, cm().copyOut("copy pj_cp to stdout with (format binary)", out));
        byte[] hdr = Arrays.copyOf(out.toByteArray(), 11);
        assertArrayEquals(new byte[]{'P', 'G', 'C', 'O', 'P', 'Y', '\n', (byte) 0xff, '\r', '\n', 0}, hdr);
        assertEquals(1000, cm().copyIn("copy pj_cp2 from stdin with (format binary)", new ByteArrayInputStream(out.toByteArray())));
        assertEquals(0, Env.scalar(c, "select count(*) from (select * from pj_cp except select * from pj_cp2) x"));
        assertEquals(0, Env.scalar(c, "select count(*) from (select * from pj_cp2 except select * from pj_cp) x"));
    }

    @Test void copyApiLowLevelAndLargeStream() throws Exception {
        CopyIn ci = cm().copyIn("copy pj_cp(id,name) from stdin");
        try {
            for (int i = 0; i < 200_000; i++) { byte[] b = (i + "\tn" + i + "\n").getBytes(StandardCharsets.UTF_8); ci.writeToCopy(b, 0, b.length); }
            assertEquals(200_000, ci.endCopy());
        } finally { if (ci.isActive()) ci.cancelCopy(); }
        assertEquals(200_000, Env.scalar(c, "select count(*) from pj_cp"));
        assertEquals(199_999L * 200_000L / 2, Env.scalar(c, "select sum(id) from pj_cp"));
        try (OutputStream os = new PGCopyOutputStream((PGConnection) c, "copy pj_cp2(id,name) from stdin with (format csv)")) {
            for (int i = 0; i < 1000; i++) os.write((i + ",\"x" + i + "\"\n").getBytes(StandardCharsets.UTF_8));
        }
        assertEquals(1000, Env.scalar(c, "select count(*) from pj_cp2"));
        // copy out through the low-level stream
        CopyOut co = cm().copyOut("copy (select id from pj_cp order by id limit 5) to stdout");
        StringBuilder sb = new StringBuilder(); byte[] row; while ((row = co.readFromCopy()) != null) sb.append(new String(row, StandardCharsets.UTF_8));
        assertEquals("0\n1\n2\n3\n4\n", sb.toString());
    }

    @Test void errorsMidCopyLeaveConnectionUsableAndRollBackOnlyThatCopy() throws Exception {
        Env.exec(c, "insert into pj_cp(id,name) values (1,'keep')");
        SQLException e = assertThrows(SQLException.class, () -> cm().copyIn("copy pj_cp(id,name) from stdin", new StringReader("2\ta\n3\tb\nnotanumber\tc\n")));
        assertEquals("22P02", e.getSQLState(), e.getMessage());
        assertEquals(1, Env.scalar(c, "select count(*) from pj_cp"), "failed COPY is atomic");
        e = assertThrows(SQLException.class, () -> cm().copyIn("copy pj_cp(id,name) from stdin", new StringReader("1\tdup\n")));
        assertEquals("23505", e.getSQLState(), e.getMessage());
        e = assertThrows(SQLException.class, () -> cm().copyIn("copy pj_cp(id,name) from stdin", new StringReader("5\ttoo\tmany\n")));
        assertEquals("22P04", e.getSQLState(), e.getMessage());
        e = assertThrows(SQLException.class, () -> cm().copyIn("copy pj_nope from stdin", new StringReader("")));
        assertEquals("42P01", e.getSQLState(), e.getMessage());
        // explicit cancel
        CopyIn ci = cm().copyIn("copy pj_cp(id,name) from stdin");
        byte[] b = "9\tx\n".getBytes(StandardCharsets.UTF_8); ci.writeToCopy(b, 0, b.length); ci.cancelCopy();
        assertEquals(1, Env.scalar(c, "select count(*) from pj_cp"));
        // inside a transaction: rollback undoes a successful COPY
        c.setAutoCommit(false);
        assertEquals(2, cm().copyIn("copy pj_cp(id,name) from stdin", new StringReader("10\ta\n11\tb\n")));
        c.rollback(); c.setAutoCommit(true);
        assertEquals(1, Env.scalar(c, "select count(*) from pj_cp"));
        // truncated binary stream
        byte[] bin = binary(10);
        e = assertThrows(SQLException.class, () -> cm().copyIn("copy pj_cp from stdin with (format binary)", new ByteArrayInputStream(Arrays.copyOf(bin, bin.length - 20))));
        assertNotNull(e.getSQLState());
        assertEquals(1, Env.scalar(c, "select count(*) from pj_cp"));
    }
}
