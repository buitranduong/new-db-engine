# accept/clients/crm: a real Jmix 2.8.1 application (vehicle-management CRM, 78 entities) against STASK and PostgreSQL 16

Task `crmcompat` (2026-10-30). The application source is NOT in this repository (it was copied to the VM by its owner: `~/crm`, tracked sources
only; its `application*.properties`, backups, `deploy/` and `web/` secrets were not copied). Nothing from it is stored here, only the scripts that
drive it. No password is stored here either: the database password is generated into `/tmp/crm-run/env.sh` and the REST login comes from
`CRM_API_USER` / `CRM_API_PASS` (a user of the application's own seed data).

Setup (all under `/tmp/crm-run`, ports 28600-28619, names `crm-*`):

| What | Where |
|---|---|
| application on STASK | port 28600 (`boot.sh stask`), database = staskd on pg port 28602 (`start-stask.sh`, `-pg-auth scram`) |
| application on PostgreSQL | port 28604 (`boot.sh pg 28604`), database = container `crm-pg` (postgres:16, port 28601, `log_statement=all`) |
| `~/crm/src/main/resources/application.properties` (not in git) | `main.datasource.*` (url from the command line, user and password from `CRM_DB_USER` / `CRM_DB_PASSWORD`), `main.datasource.hikari.connection-init-sql` as in production, `server.port`, `spring.cache.jcache.provider=com.hazelcast.cache.HazelcastMemberCachingProvider` + `spring.cache.jcache.config=classpath:hazelcast.yaml` (otherwise the JCache provider picks the Hazelcast client and the context fails), `jmix.ui.login-view-id=LoginView`, `jmix.ui.main-view-id=MainView`, random JWT secret, OTLP exporters off |
| Liquibase | the application's own `changelog-release.xml` (baseline + 72 releases; the dev `changelog.xml` is an incremental set that does not start from an empty database). Two variants were made next to it (not in git): `changelog-crmcompat.xml` = the release changelog with `functions.xml` moved to the end (as shipped it runs before TASK exists and fails on PostgreSQL too), and `changelog-crmcompat-stask.xml` = the same without the one changeset that creates a PL/pgSQL trigger function |

Scripts:

- `boot.sh stask|pg [port]` runs `./gradlew bootRun` with JDK 21 from `~/rc-tools/jdk`; `iter.sh` restarts staskd on a fresh data directory, boots the application and prints the first error.
- `replay.py PORT` re-runs the statements the application sent to PostgreSQL (from `docker logs crm-pg`, parameters substituted) against STASK one by one and prints each failure: finds all DDL gaps in one pass instead of one per boot.
- `catdiff.sh` diffs tables, columns (information_schema), constraints (`pg_get_constraintdef`), indexes (`pg_indexes`), sequences and functions between the two databases.
- `apidiff.py PGPORT STASKPORT OUTDIR`: every GET endpoint without path variables found in the controllers (194), with 5 query variants (paging, sort, search), both backends, normalized JSON diffed.
- `crud.py PGPORT STASKPORT OUTDIR`: 227 steps through the REST API the React UI uses: create/update/delete/bulk-delete, duplicate keys, stale optimistic-lock version (409), paging, sort, accent-insensitive search (`unaccent_lower`), users/roles/menu/reports reads. `reset.sh` empties the catalog tables between runs.

Result and gaps: `docs/14_plan_target_actual.md`, Appendix V.
