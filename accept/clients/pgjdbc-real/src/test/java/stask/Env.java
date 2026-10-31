package stask;

import java.sql.*;
import java.util.Properties;

/** Connection helper. -Dstask.url=jdbc:postgresql://host:port/db (env STASK_URL), user/password from env. */
final class Env {
    static String base() { String u = System.getProperty("stask.url", ""); if (u.isEmpty()) throw new IllegalStateException("set STASK_URL"); return u; }
    static Connection connect(String params) throws SQLException {
        Properties p = new Properties();
        p.setProperty("user", System.getProperty("stask.user", "app"));
        p.setProperty("password", System.getProperty("stask.password", ""));
        p.setProperty("sslmode", "disable");
        String u = base();
        if (params != null && !params.isEmpty()) u += (u.contains("?") ? "&" : "?") + params;
        return DriverManager.getConnection(u, p);
    }
    static Connection connect() throws SQLException { return connect(""); }
    static Connection connectTo(String url, String params) throws SQLException {
        Properties p = new Properties();
        p.setProperty("user", System.getProperty("stask.user", "app"));
        p.setProperty("password", System.getProperty("stask.password", ""));
        p.setProperty("sslmode", "disable");
        return DriverManager.getConnection(params == null || params.isEmpty() ? url : url + (url.contains("?") ? "&" : "?") + params, p);
    }
    static void exec(Connection c, String sql) throws SQLException { try (Statement s = c.createStatement()) { s.execute(sql); } }
    static long scalar(Connection c, String sql) throws SQLException {
        try (Statement s = c.createStatement(); ResultSet r = s.executeQuery(sql)) { r.next(); return r.getLong(1); }
    }
    static String str(Connection c, String sql) throws SQLException {
        try (Statement s = c.createStatement(); ResultSet r = s.executeQuery(sql)) { r.next(); return r.getString(1); }
    }
}
