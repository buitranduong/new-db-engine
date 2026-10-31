import java.io.StringReader;
import java.io.StringWriter;
import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.Statement;
import org.postgresql.PGConnection;
import org.postgresql.copy.CopyManager;

/** pgjdbc smoke: connect, SELECT 1, PreparedStatement, COPY. Env: PGHOST PGPORT PGUSER PGPASSWORD PGDATABASE,
 *  or JDBC_URL for a full URL (for example with targetServerType=primary and several hosts). */
public class Smoke {
    public static void main(String[] a) throws Exception {
        String url = System.getenv("JDBC_URL");
        if (url == null) {
            url = "jdbc:postgresql://" + env("PGHOST", "127.0.0.1") + ":" + env("PGPORT", "5432") + "/" + env("PGDATABASE", "postgres")
                + "?sslmode=" + env("PGSSLMODE", "disable") + "&prepareThreshold=1";
        }
        try (Connection c = DriverManager.getConnection(url, env("PGUSER", "postgres"), env("PGPASSWORD", ""))) {
            c.setAutoCommit(true);
            try (Statement s = c.createStatement(); ResultSet r = s.executeQuery("SELECT 1")) {
                if (!r.next() || r.getInt(1) != 1) throw new Exception("select 1");
            }
            try (Statement s = c.createStatement()) {
                s.execute("DROP TABLE IF EXISTS suite_smoke");
                s.execute("CREATE TABLE suite_smoke (id int PRIMARY KEY, v text)");
            }
            try {
                try (PreparedStatement p = c.prepareStatement("INSERT INTO suite_smoke VALUES (?, ?)")) {
                    p.setInt(1, 1); p.setString(2, "prepared"); p.executeUpdate();
                }
                CopyManager cm = ((PGConnection) c).getCopyAPI();
                long n = cm.copyIn("COPY suite_smoke FROM STDIN", new StringReader("2\tcopied\n3\tcopied too\n"));
                if (n != 2) throw new Exception("copyIn " + n);
                StringWriter w = new StringWriter();
                cm.copyOut("COPY suite_smoke TO STDOUT", w);
                if (w.toString().split("\n").length != 3) throw new Exception("copyOut " + w);
                try (PreparedStatement p = c.prepareStatement("SELECT count(*) FROM suite_smoke WHERE v LIKE ?")) {
                    p.setString(1, "copied%");
                    try (ResultSet r = p.executeQuery()) {
                        if (!r.next() || r.getInt(1) != 2) throw new Exception("param query");
                    }
                }
            } finally {
                try (Statement s = c.createStatement()) { s.execute("DROP TABLE IF EXISTS suite_smoke"); }
            }
        }
        System.out.println("OK pgjdbc");
    }

    static String env(String k, String d) { String v = System.getenv(k); return v == null || v.isEmpty() ? d : v; }
}
