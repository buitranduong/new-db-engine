import java.sql.*;

/** pgjdbc: setFetchSize (portal suspension) and explicit DECLARE/FETCH cursors, FOR UPDATE SKIP LOCKED. */
public class CursorsJdbc {
    public static void main(String[] a) throws Exception {
        String url = a[0];
        int n = a.length > 1 ? Integer.parseInt(a[1]) : 50000;
        try (Connection c = DriverManager.getConnection(url, "app", "")) {
            try (Statement s = c.createStatement()) {
                s.execute("DROP TABLE IF EXISTS jcur");
                s.execute("CREATE TABLE jcur (id BIGINT PRIMARY KEY, v TEXT)");
                s.execute("INSERT INTO jcur SELECT g, 'v' || g FROM generate_series(1, " + n + ") AS g");
            }
            c.setAutoCommit(false);
            // 1. fetch size: pgjdbc sends Execute with a row limit and reads the portal in batches
            long cnt = 0;
            try (PreparedStatement ps = c.prepareStatement("SELECT id, v FROM jcur ORDER BY id")) {
                ps.setFetchSize(500);
                try (ResultSet rs = ps.executeQuery()) {
                    while (rs.next()) {
                        cnt++;
                        if (rs.getLong(1) != cnt) throw new RuntimeException("order " + cnt);
                    }
                }
            }
            if (cnt != n) throw new RuntimeException("fetch size rows " + cnt);
            c.commit();
            // 2. explicit cursor
            try (Statement s = c.createStatement()) {
                s.execute("DECLARE jc SCROLL CURSOR FOR SELECT id FROM jcur ORDER BY id");
                long total = 0;
                while (true) {
                    int got = 0;
                    try (ResultSet rs = s.executeQuery("FETCH 1000 FROM jc")) {
                        while (rs.next()) { got++; }
                    }
                    total += got;
                    if (got < 1000) break;
                }
                if (total != n) throw new RuntimeException("declare rows " + total);
                try (ResultSet rs = s.executeQuery("FETCH PRIOR FROM jc")) {
                    if (!rs.next() || rs.getLong(1) != n) throw new RuntimeException("prior " + n);
                }
                s.execute("CLOSE jc");
            }
            c.commit();
            // 3. SKIP LOCKED between two connections
            try (Connection c2 = DriverManager.getConnection(url, "app", "")) {
                c2.setAutoCommit(false);
                try (Statement s1 = c.createStatement(); Statement s2 = c2.createStatement()) {
                    ResultSet r1 = s1.executeQuery("SELECT id FROM jcur ORDER BY id LIMIT 3 FOR UPDATE SKIP LOCKED");
                    long[] first = new long[3];
                    for (int i = 0; r1.next(); i++) first[i] = r1.getLong(1);
                    ResultSet r2 = s2.executeQuery("SELECT id FROM jcur ORDER BY id LIMIT 3 FOR UPDATE SKIP LOCKED");
                    for (int i = 0; r2.next(); i++) if (r2.getLong(1) == first[0] || r2.getLong(1) <= 3) throw new RuntimeException("duplicate row " + r2.getLong(1));
                    try {
                        s2.executeQuery("SELECT id FROM jcur WHERE id = 1 FOR UPDATE NOWAIT");
                        throw new RuntimeException("NOWAIT did not fail");
                    } catch (SQLException e) {
                        if (!"55P03".equals(e.getSQLState())) throw e;
                    }
                }
                c2.rollback();
            }
            c.rollback();
            try (Statement s = c.createStatement(); ResultSet rs = s.executeQuery("SELECT current_user")) {
                rs.next();
                if (!"app".equals(rs.getString(1))) throw new RuntimeException("current_user " + rs.getString(1));
            }
        }
        System.out.println("JDBC cursors OK");
    }
}
