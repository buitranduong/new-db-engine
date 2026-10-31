package stask;

import static org.junit.jupiter.api.Assertions.*;

import jakarta.persistence.*;
import java.util.*;
import java.util.concurrent.*;
import java.util.concurrent.atomic.*;
import org.hibernate.LockMode;
import org.hibernate.LockOptions;
import org.hibernate.Session;
import org.hibernate.SessionFactory;
import org.hibernate.StaleObjectStateException;
import org.hibernate.Transaction;
import org.hibernate.annotations.JdbcTypeCode;
import org.hibernate.cfg.Configuration;
import org.hibernate.type.SqlTypes;
import org.junit.jupiter.api.*;

/** Real Hibernate 6 on the real pgjdbc against STASK: DDL generation, batching, optimistic and pessimistic locking, bytea. */
class HibernateTest {
    @Entity(name = "Account") @Table(name = "hb_account")
    public static class Account {
        @Id @GeneratedValue(strategy = GenerationType.SEQUENCE, generator = "acc_seq") @SequenceGenerator(name = "acc_seq", sequenceName = "hb_account_seq", allocationSize = 50)
        public Long id;
        @Version public int version;
        @Column(nullable = false, length = 80) public String owner;
        public long balance;
        public java.time.Instant touched;
        public java.time.LocalDate day;
        public java.math.BigDecimal amount;
        public Account() { }
        public Account(String o, long b) { owner = o; balance = b; }
    }

    @Entity(name = "Doc") @Table(name = "hb_doc")
    public static class Doc {
        @Id @GeneratedValue(strategy = GenerationType.IDENTITY) public Long id;
        @Column(name = "title") public String title;
        @Lob @JdbcTypeCode(SqlTypes.VARBINARY) public byte[] payload;
        @JdbcTypeCode(SqlTypes.ARRAY) @Column(name = "tags", columnDefinition = "text[]") public String[] tags;
        public UUID ref;
    }

    @Entity(name = "Child") @Table(name = "hb_child")
    public static class Child {
        @Id public UUID id = UUID.randomUUID();
        @ManyToOne(fetch = FetchType.LAZY, optional = false) @JoinColumn(name = "account_id") public Account account;
        public String note;
    }

    static SessionFactory sf;

    @BeforeAll static void up() {
        Configuration cfg = new Configuration();
        cfg.setProperty("hibernate.connection.url", Env.base());
        cfg.setProperty("hibernate.connection.username", System.getProperty("stask.user", "app"));
        cfg.setProperty("hibernate.connection.password", System.getProperty("stask.password", ""));
        cfg.setProperty("hibernate.connection.driver_class", "org.postgresql.Driver");
        cfg.setProperty("hibernate.connection.provider_disables_autocommit", "false");
        cfg.setProperty("hibernate.hbm2ddl.auto", "create-drop");
        cfg.setProperty("hibernate.jdbc.batch_size", "50");
        cfg.setProperty("hibernate.order_inserts", "true");
        cfg.setProperty("hibernate.order_updates", "true");
        cfg.setProperty("hibernate.jdbc.batch_versioned_data", "true");
        cfg.setProperty("hibernate.generate_statistics", "true");
        cfg.addAnnotatedClass(Account.class).addAnnotatedClass(Doc.class).addAnnotatedClass(Child.class);
        sf = cfg.buildSessionFactory();
    }
    @AfterAll static void down() { if (sf != null) sf.close(); }

    long newAccount(String owner, long bal) {
        try (Session s = sf.openSession()) { Transaction t = s.beginTransaction(); Account a = new Account(owner, bal); s.persist(a); t.commit(); return a.id; }
    }

    @Test void batchInsertsUseJdbcBatching() {
        sf.getStatistics().clear();
        try (Session s = sf.openSession()) {
            Transaction t = s.beginTransaction();
            for (int i = 0; i < 500; i++) {
                Account a = new Account("batch" + i, i); a.touched = java.time.Instant.parse("2024-05-01T10:00:00.123456Z"); a.day = java.time.LocalDate.of(2024, 5, 1); a.amount = new java.math.BigDecimal("12.34");
                s.persist(a);
                Child c = new Child(); c.account = a; c.note = "n" + i; s.persist(c);
                if (i % 100 == 99) { s.flush(); s.clear(); }
            }
            t.commit();
        }
        assertEquals(500, sf.getStatistics().getEntityInsertCount() / 2);
        try (Session s = sf.openSession()) {
            assertEquals(500L, s.createQuery("select count(a) from Account a where a.owner like 'batch%'", Long.class).getSingleResult());
            assertEquals(500L, s.createQuery("select count(c) from Child c join c.account a where a.amount = 12.34", Long.class).getSingleResult());
            Account a = s.createQuery("from Account a where a.owner = 'batch7'", Account.class).getSingleResult();
            assertEquals(java.time.Instant.parse("2024-05-01T10:00:00.123456Z"), a.touched);
            assertEquals(java.time.LocalDate.of(2024, 5, 1), a.day);
            assertEquals(0, new java.math.BigDecimal("12.34").compareTo(a.amount));
        }
        try (Session s = sf.openSession()) { // batched updates with @Version
            Transaction t = s.beginTransaction();
            List<Account> l = s.createQuery("from Account a where a.owner like 'batch%'", Account.class).list();
            for (Account a : l) a.balance += 1000;
            t.commit();
            assertEquals(500, l.size());
        }
        try (Session s = sf.openSession()) {
            assertEquals(500L, s.createQuery("select count(a) from Account a where a.owner like 'batch%' and a.version = 1 and a.balance >= 1000", Long.class).getSingleResult());
        }
        try (Session s = sf.openSession()) { Transaction t = s.beginTransaction(); assertEquals(500, s.createMutationQuery("delete from Child").executeUpdate()); assertEquals(500, s.createMutationQuery("delete from Account a where a.owner like 'batch%'").executeUpdate()); t.commit(); }
    }

    @Test void binaryParametersAndArraysAndIdentity() {
        byte[] big = new byte[1_500_000]; new Random(42).nextBytes(big);
        long id;
        try (Session s = sf.openSession()) {
            Transaction t = s.beginTransaction(); Doc d = new Doc(); d.title = "d"; d.payload = big; d.tags = new String[]{"a", "b c", null}; d.ref = UUID.randomUUID(); s.persist(d); t.commit(); id = d.id; assertNotNull(d.id);
        }
        try (Session s = sf.openSession()) {
            Doc d = s.find(Doc.class, id); assertArrayEquals(big, d.payload); assertArrayEquals(new String[]{"a", "b c", null}, d.tags); assertNotNull(d.ref);
            assertEquals(big.length, ((Number) s.createNativeQuery("select length(payload) from hb_doc where id = :i", Object.class).setParameter("i", id).getSingleResult()).intValue());
        }
        try (Session s = sf.openSession()) { // empty and null bytea
            Transaction t = s.beginTransaction(); Doc d = new Doc(); d.title = "e"; d.payload = new byte[0]; s.persist(d); Doc n = new Doc(); n.title = "n"; s.persist(n); t.commit();
            s.clear(); assertEquals(0, s.find(Doc.class, d.id).payload.length); assertNull(s.find(Doc.class, n.id).payload);
        }
    }

    @Test void optimisticLockingDetectsConcurrentUpdate() {
        long id = newAccount("opt", 100);
        Session s1 = sf.openSession(), s2 = sf.openSession();
        try {
            Transaction t1 = s1.beginTransaction(), t2 = s2.beginTransaction();
            Account a1 = s1.find(Account.class, id), a2 = s2.find(Account.class, id);
            a1.balance = 200; t1.commit();
            a2.balance = 300;
            Exception ex = assertThrows(Exception.class, () -> { s2.flush(); t2.commit(); });
            assertTrue(ex instanceof OptimisticLockException || ex instanceof StaleObjectStateException || ex.getCause() instanceof StaleObjectStateException || ex.getCause() instanceof OptimisticLockException || ex.toString().contains("Row was updated or deleted"), ex.toString());
            if (t2.isActive()) t2.rollback();
        } finally { s1.close(); s2.close(); }
        try (Session s = sf.openSession()) { Account a = s.find(Account.class, id); assertEquals(200, a.balance); assertEquals(1, a.version); }
        // OPTIMISTIC_FORCE_INCREMENT and explicit lock(OPTIMISTIC)
        try (Session s = sf.openSession()) {
            Transaction t = s.beginTransaction(); Account a = s.find(Account.class, id, LockModeType.OPTIMISTIC_FORCE_INCREMENT); t.commit(); assertEquals(2, a.version);
        }
        // many threads increment with retry on conflict; final balance equals number of successful increments
        AtomicInteger done = new AtomicInteger();
        ExecutorService ex = Executors.newFixedThreadPool(8);
        List<Future<?>> fs = new ArrayList<>();
        for (int w = 0; w < 8; w++) fs.add(ex.submit(() -> {
            for (int i = 0; i < 10; i++) {
                while (true) {
                    try (Session s = sf.openSession()) {
                        Transaction t = s.beginTransaction(); Account a = s.find(Account.class, id); a.balance++; t.commit(); done.incrementAndGet(); break;
                    } catch (OptimisticLockException | StaleObjectStateException | jakarta.persistence.RollbackException e) { /* retry */ }
                    catch (Exception e) { if (String.valueOf(e).contains("Row was updated or deleted") || String.valueOf(e.getCause()).contains("Row was updated or deleted")) continue; throw e; }
                }
            }
            return null;
        }));
        for (Future<?> f : fs) { try { f.get(120, TimeUnit.SECONDS); } catch (Exception e) { fail(e); } }
        ex.shutdown();
        try (Session s = sf.openSession()) { assertEquals(80, done.get()); assertEquals(200 + 80, s.find(Account.class, id).balance); }
    }

    @Test void pessimisticWriteBlocksAndSeesCommittedValue() throws Exception {
        long id = newAccount("pess", 10);
        CountDownLatch locked = new CountDownLatch(1);
        AtomicLong bAcquiredAfterMs = new AtomicLong(-1), bSaw = new AtomicLong(-1);
        long[] aCommit = new long[1];
        Thread a = new Thread(() -> {
            try (Session s = sf.openSession()) {
                Transaction t = s.beginTransaction();
                Account acc = s.find(Account.class, id, LockModeType.PESSIMISTIC_WRITE);
                locked.countDown();
                Thread.sleep(1500);
                acc.balance = 99;
                s.flush();
                aCommit[0] = System.nanoTime();
                t.commit();
            } catch (Exception e) { throw new RuntimeException(e); }
        });
        a.start();
        assertTrue(locked.await(20, TimeUnit.SECONDS));
        long t0 = System.nanoTime();
        try (Session s = sf.openSession()) {
            Transaction t = s.beginTransaction();
            Account acc = s.find(Account.class, id, LockModeType.PESSIMISTIC_WRITE);   // must wait for A
            bAcquiredAfterMs.set((System.nanoTime() - t0) / 1_000_000); bSaw.set(acc.balance);
            assertTrue(System.nanoTime() >= aCommit[0], "B got the row lock only after A committed");
            acc.balance += 1; t.commit();
        }
        a.join();
        assertTrue(bAcquiredAfterMs.get() >= 1000, "B waited for A's FOR UPDATE: " + bAcquiredAfterMs);
        assertEquals(99, bSaw.get(), "B sees A's committed value after the wait (READ COMMITTED re-read)");
        try (Session s = sf.openSession()) { assertEquals(100, s.find(Account.class, id).balance); }
    }

    @Test void pessimisticNoWaitSkipLockedAndTimeout() throws Exception {
        long id = newAccount("nowait", 1);
        long id2 = newAccount("nowait2", 2);
        CountDownLatch locked = new CountDownLatch(1), release = new CountDownLatch(1);
        Thread holder = new Thread(() -> {
            try (Session s = sf.openSession()) {
                Transaction t = s.beginTransaction();
                s.find(Account.class, id, LockModeType.PESSIMISTIC_WRITE);
                locked.countDown();
                release.await(30, TimeUnit.SECONDS);
                t.commit();
            } catch (Exception e) { throw new RuntimeException(e); }
        });
        holder.start();
        assertTrue(locked.await(20, TimeUnit.SECONDS));
        try {
            // NOWAIT
            try (Session s = sf.openSession()) {
                Transaction t = s.beginTransaction();
                Exception ex = assertThrows(Exception.class, () -> s.find(Account.class, id, LockModeType.PESSIMISTIC_WRITE, Map.of("jakarta.persistence.lock.timeout", 0)));
                assertTrue(ex instanceof jakarta.persistence.PessimisticLockException || ex instanceof LockTimeoutException || ex instanceof org.hibernate.PessimisticLockException || ex instanceof org.hibernate.exception.LockTimeoutException || ex instanceof org.hibernate.exception.LockAcquisitionException || ex.getCause() instanceof org.hibernate.exception.LockAcquisitionException,
                        "NOWAIT conflict surfaces as a lock exception (55P03): " + ex);
                if (t.isActive()) t.rollback();
            }
            // SKIP LOCKED sees the unlocked row only
            try (Session s = sf.openSession()) {
                Transaction t = s.beginTransaction();
                List<Account> l = s.createQuery("from Account a where a.id in (:a, :b) order by a.id", Account.class).setParameter("a", id).setParameter("b", id2)
                        .setLockOptions(new LockOptions(LockMode.PESSIMISTIC_WRITE).setTimeOut(LockOptions.SKIP_LOCKED)).list();
                assertEquals(1, l.size(), "locked row skipped");
                assertEquals(id2, l.get(0).id);
                t.rollback();
            }
            // PESSIMISTIC_READ (FOR SHARE) on the other row is compatible with another share lock
            try (Session s1 = sf.openSession(); Session s2 = sf.openSession()) {
                Transaction t1 = s1.beginTransaction(), t2 = s2.beginTransaction();
                s1.find(Account.class, id2, LockModeType.PESSIMISTIC_READ); s2.find(Account.class, id2, LockModeType.PESSIMISTIC_READ);
                t1.commit(); t2.commit();
            }
            // lock + FORCE_INCREMENT
            try (Session s = sf.openSession()) { Transaction t = s.beginTransaction(); Account a = s.find(Account.class, id2, LockModeType.PESSIMISTIC_FORCE_INCREMENT); int v = a.version; t.commit(); assertEquals(v, a.version); }
        } finally { release.countDown(); holder.join(); }
    }

    @Test void deadlockIsDetectedAndOneSideAborts() throws Exception {
        long x = newAccount("dl-x", 1), y = newAccount("dl-y", 1);
        CyclicBarrier bar = new CyclicBarrier(2);
        ExecutorService ex = Executors.newFixedThreadPool(2);
        Callable<String> w1 = () -> run(x, y, bar), w2 = () -> run(y, x, bar);
        List<Future<String>> r = ex.invokeAll(List.of(w1, w2), 60, TimeUnit.SECONDS);
        ex.shutdown();
        List<String> out = new ArrayList<>(); for (Future<String> f : r) out.add(f.get());
        assertEquals(1, out.stream().filter("ok"::equals).count(), "exactly one transaction survives the deadlock: " + out);
        assertEquals(1, out.stream().filter(o -> o.contains("40P01") || o.toLowerCase().contains("deadlock")).count(), out.toString());
    }
    String run(long first, long second, CyclicBarrier bar) {
        try (Session s = sf.openSession()) {
            Transaction t = s.beginTransaction();
            s.find(Account.class, first, LockModeType.PESSIMISTIC_WRITE);
            bar.await(20, TimeUnit.SECONDS);
            s.find(Account.class, second, LockModeType.PESSIMISTIC_WRITE);
            t.commit(); return "ok";
        } catch (Exception e) { Throwable c = e; StringBuilder sb = new StringBuilder(); while (c != null) { sb.append(c).append(" | "); if (c instanceof java.sql.SQLException q) sb.append(q.getSQLState()); c = c.getCause(); } return sb.toString(); }
    }
}
