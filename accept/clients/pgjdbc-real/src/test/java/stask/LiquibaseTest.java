package stask;

import static org.junit.jupiter.api.Assertions.*;
import static org.junit.jupiter.api.Assumptions.assumeFalse;

import java.io.File;
import java.sql.*;
import liquibase.*;
import liquibase.database.*;
import liquibase.database.jvm.JdbcConnection;
import liquibase.resource.*;
import org.junit.jupiter.api.*;

/** Liquibase update / rerun / rollback / tag / validate / status on STASK, with our own rollback-capable changelog and the CRM's real one. */
class LiquibaseTest {
    static Connection c;
    @BeforeAll static void up() throws Exception { c = Env.connect(); }
    @AfterAll static void down() throws Exception {
        for (String t : new String[]{"lb_c", "lb_p", "lb_x", "lb_seed", "lb_loaded", "databasechangelog", "databasechangeloglock"}) Env.exec(c, "drop table if exists " + t + " cascade");
        c.close();
    }
    Liquibase lb(String file, ResourceAccessor ra) throws Exception {
        Database db = DatabaseFactory.getInstance().findCorrectDatabaseImplementation(new JdbcConnection(Env.connect()));
        return new Liquibase(file, ra, db);
    }
    ResourceAccessor mine() throws Exception { return new DirectoryResourceAccessor(new File("src/test/resources").getAbsoluteFile()); }

    long n(String t) throws SQLException { return Env.scalar(c, "select count(*) from " + t); }
    boolean exists(String t) throws SQLException { return Env.scalar(c, "select count(*) from information_schema.tables where table_name='" + t + "'") > 0; }

    @Test void updateRerunTagRollbackUpdateAgain() throws Exception {
        for (String t : new String[]{"lb_c", "lb_p", "lb_x", "lb_seed", "lb_loaded", "databasechangelog", "databasechangeloglock"}) Env.exec(c, "drop table if exists " + t + " cascade");
        try (Liquibase l = lb("lb/changelog.xml", mine())) {
            l.validate();
            l.update(new Contexts(), new LabelExpression());
            assertEquals(8, n("databasechangelog"));
            assertTrue(exists("lb_p") && exists("lb_c") && exists("lb_x"));
            assertEquals(3, n("lb_seed"));
            assertEquals(2, n("lb_loaded"));
        }
        try (Liquibase l = lb("lb/changelog.xml", mine())) {          // rerun: no-op
            l.update(new Contexts(), new LabelExpression());
            assertEquals(8, n("databasechangelog"));
            l.validate();
        }
        try (Liquibase l = lb("lb/changelog.xml", mine())) {          // rollback to tag, tables created later disappear
            l.rollback("v1", new Contexts());
            assertEquals(3, n("databasechangelog"), "the tag changeset itself is after the tag and is rolled back too");
            assertTrue(exists("lb_p") && exists("lb_c"));
            assertFalse(exists("lb_x"), "table of a later changeset is rolled back");
            assertFalse(exists("lb_loaded"));
            assertEquals(0, Env.scalar(c, "select count(*) from information_schema.columns where table_name='lb_c' and column_name='extra'"), "addColumn rolled back");
            assertEquals(0, Env.scalar(c, "select count(*) from pg_indexes where indexname='lb_c_extra_idx'"));
            assertEquals(3, n("lb_seed"), "changeset 3 is before the tag and stays");
        }
        try (Liquibase l = lb("lb/changelog.xml", mine())) {          // and forward again
            l.update(new Contexts(), new LabelExpression());
            assertEquals(8, n("databasechangelog"));
            assertTrue(exists("lb_x") && exists("lb_loaded"));
            l.rollback(2, "");                                          // rollbackCount
            assertEquals(6, n("databasechangelog"));
            l.update(new Contexts(), new LabelExpression());
            l.updateTestingRollback(new Contexts(), new LabelExpression());   // update, rollback, update again for the last unrun set (none) is a no-op
            assertEquals(8, n("databasechangelog"));
        }
    }
}
