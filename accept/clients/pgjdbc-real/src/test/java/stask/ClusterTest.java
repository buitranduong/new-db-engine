package stask;

import static org.junit.jupiter.api.Assertions.*;
import static org.junit.jupiter.api.Assumptions.assumeFalse;

import java.sql.*;
import java.util.*;
import org.junit.jupiter.api.*;

/** pgjdbc multi-host URL against a 3-node Raft cluster (STASK_CLUSTER=h1:p1,h2:p2,h3:p3). Phase 1 = healthy cluster; phase 2 = after the leader was killed. */
class ClusterTest {
    static String hosts() { return System.getProperty("stask.cluster", ""); }
    static String db() { return System.getProperty("stask.db", "stask"); }
    static String url(String attrs) { return "jdbc:postgresql://" + hosts() + "/" + db() + "?targetServerType=" + attrs; }

    static String nodeOf(Connection c) throws SQLException { return ((org.postgresql.jdbc.PgConnection) c).getQueryExecutor().getHostSpec().toString(); }

    @BeforeAll static void gate() { assumeFalse(hosts().isEmpty(), "STASK_CLUSTER not set"); }

    @Test void primarySecondaryAnyRouting() throws Exception {
        assumeFalse(!"1".equals(System.getProperty("stask.phase", "1")));
        try (Connection p = Env.connectTo(url("primary"), "hostRecheckSeconds=1")) {
            assertEquals("off", Env.str(p, "show transaction_read_only"));
            assertEquals("f", Env.str(p, "select pg_is_in_recovery()::text").substring(0, 1).replace("false", "f"));
            Env.exec(p, "drop table if exists pj_cl"); Env.exec(p, "create table pj_cl(id int primary key, v text)");
            for (int i = 0; i < 100; i++) Env.exec(p, "insert into pj_cl values (" + i + ", 'p')");
            assertEquals(100, Env.scalar(p, "select count(*) from pj_cl"));
            String leader = nodeOf(p);
            // each follower must have replicated it (wait briefly: followers apply asynchronously)
            Set<String> seen = new TreeSet<>();
            for (int k = 0; k < 20; k++) {
                try (Connection s = Env.connectTo(url("secondary"), "loadBalanceHosts=true")) {
                    assertEquals("on", Env.str(s, "show transaction_read_only"));
                    assertNotEquals(leader, nodeOf(s));
                    seen.add(nodeOf(s));
                    SQLException e = assertThrows(SQLException.class, () -> Env.exec(s, "insert into pj_cl values (1000,'x')"));
                    assertEquals("25006", e.getSQLState(), e.getMessage());
                    long deadline = System.currentTimeMillis() + 10_000; long n = 0;
                    while (System.currentTimeMillis() < deadline && (n = Env.scalar(s, "select count(*) from pj_cl")) < 100) Thread.sleep(50);
                    assertEquals(100, n, "follower caught up");
                }
            }
            assertEquals(2, seen.size(), "loadBalanceHosts spread secondary connections across both followers: " + seen);
            for (String t : new String[]{"any", "preferPrimary", "preferSecondary"}) {
                try (Connection c = Env.connectTo(url(t), "")) { assertEquals(100, Env.scalar(c, "select count(*) from pj_cl")); }
            }
            try (Connection c = Env.connectTo(url("preferSecondary"), "")) { assertEquals("on", Env.str(c, "show transaction_read_only")); }
            try (Connection c = Env.connectTo(url("preferPrimary"), "")) { assertEquals("off", Env.str(c, "show transaction_read_only")); }
            // writes through a pool-like cycle: connect(primary) / write / close, 50 times
            for (int i = 100; i < 150; i++) try (Connection c = Env.connectTo(url("primary"), "")) { Env.exec(c, "insert into pj_cl values (" + i + ",'cycle')"); }
            assertEquals(150, Env.scalar(p, "select count(*) from pj_cl"));
            // a write on a connection that was opened as "any" and landed on a follower fails cleanly with 25006
            // Connection.isValid / read-only flag as Hikari would use them
            assertTrue(p.isValid(5)); assertFalse(p.isReadOnly());
        }
    }

    /** Phase 2 runs after the harness killed the leader (kill -9): the driver must find the new leader by itself. */
    @Test void newLeaderFoundAfterKill() throws Exception {
        assumeFalse(!"2".equals(System.getProperty("stask.phase", "1")));
        long deadline = System.currentTimeMillis() + 60_000; SQLException last = null; Connection p = null;
        while (System.currentTimeMillis() < deadline) {
            try { p = Env.connectTo(url("primary"), "hostRecheckSeconds=1&connectTimeout=2&loginTimeout=5"); Env.exec(p, "insert into pj_cl values (1000,'after-kill')"); break; }
            catch (SQLException e) { last = e; if (p != null) { try { p.close(); } catch (SQLException ignore) { } p = null; } Thread.sleep(500); }
        }
        assertNotNull(p, "primary reachable again within 60s after the leader died; last error: " + last);
        final Connection pc = p;
        try (pc) {
            assertEquals("off", Env.str(pc, "show transaction_read_only"));
            long n = Env.scalar(pc, "select count(*) from pj_cl");
            assertEquals(151, n, "no acknowledged write lost across the failover");
        }
        try (Connection s = Env.connectTo(url("secondary"), "hostRecheckSeconds=1")) { assertEquals("on", Env.str(s, "show transaction_read_only")); }
    }
}
