# stask-m2 report: pg16

- run: 2026-11-01T05:52:05Z, harness commit 6afb539, **INVALID: LOAD NOT APPLIED**
- scale: 10000000 sims (43000200 rows), seed stask1; load: T=4 P=4 R=2, import always every 5s, hot set 200 at 30%; 123 s elapsed
- machine: 8 CPUs, load1 start 1.00 / max 12.87 / end 11.78, disk free 4.7 GB

## LOAD NOT APPLIED

**This run is INVALID.** The load it was configured to apply did not happen, so "all oracles held" below compares empty sets and proves nothing, and no number of this report may be quoted.

- r2_page: 24 successes, minimum 30 (-r 2, -min-r2)

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 19 | 0 |  |
| no_orphan_order_items | 5 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 19 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 17 | 0 |  |
| r2_pages_prefix_consistent_frozen | 17 | 0 |  |
| reserved_stock_equals_open_items | 19 | 0 |  |
| stock_invariant_q13 | 19 | 0 |  |
| tear_no_order_without_items | 6 | 0 |  |
| tear_no_orphan_items | 6 | 0 |  |
| tear_order_total_equals_items | 6 | 0 |  |
| tear_q13_at_one_snapshot | 8 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 5 | 0 |  |
| tear_reserved_equals_open_items | 6 | 0 |  |

No oracle was violated, but the run is INVALID (load not applied).

## Latency and throughput per class (scale 10000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 9 | map[] | 0.1 | 9027.82 | 11837.68 | 11837.68 | 11837.68 |
| r1_keyset | 46249 | map[] | 374.6 | 3.19 | 7.86 | 11.24 | 28.55 |
| r1_point | 46757 | map[] | 378.7 | 1.34 | 4.70 | 6.83 | 17.12 |
| r2_page | 24 | map[53100:96] | 0.2 | 7809.44 | 9084.56 | 9789.61 | 9789.61 |
| r_join | 8 | map[53100:8] | 0.1 | 2578.48 | 21497.52 | 21497.52 | 21497.52 |
| t_cancel | 4792 | map[] | 38.8 | 10.05 | 17.89 | 23.81 | 62.32 |
| t_order | 9739 | map[] | 78.9 | 11.50 | 20.70 | 27.57 | 9403.48 |
| t_pay | 4947 | map[] | 40.1 | 9.99 | 18.24 | 23.25 | 59.71 |
| tear_tx | 4 | map[] | 0.0 | 18621.02 | 19657.39 | 19657.39 | 19657.39 |

## Error samples (up to 3 distinct messages per class, full text; first_s = seconds into the measured window)

| class | code | count | first_s | message |
|---|---|---|---|---|
| r2_page | 53100 | 1 | 37.3 | ERROR: could not resize shared memory segment "/PostgreSQL.570257630" to 12615680 bytes: No space left on device (SQLSTATE 53100) |
| r2_page | 53100 | 1 | 44.3 | ERROR: could not resize shared memory segment "/PostgreSQL.3860281854" to 4194304 bytes: No space left on device (SQLSTATE 53100) |
| r2_page | 53100 | 1 | 44.4 | ERROR: could not resize shared memory segment "/PostgreSQL.1968102716" to 4194304 bytes: No space left on device (SQLSTATE 53100) |
| r_join | 53100 | 1 | 45.0 | ERROR: could not resize shared memory segment "/PostgreSQL.1492079826" to 4194304 bytes: No space left on device (SQLSTATE 53100) |
| r_join | 53100 | 1 | 46.1 | ERROR: could not resize shared memory segment "/PostgreSQL.378848986" to 4194304 bytes: No space left on device (SQLSTATE 53100) |
| r_join | 53100 | 1 | 84.9 | ERROR: could not resize shared memory segment "/PostgreSQL.1529516226" to 4194304 bytes: No space left on device (SQLSTATE 53100) |

## Import shape

shape that ran: **single (1% in ONE transaction, worst case)**; every import re-prices 1% of the sims (100000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 9 ok, errors map[], p50 9028 ms, p95 11838 ms, p99 11838 ms, max 11838 ms; batches 9, rows 810000

## Counters

- import_batches = 9
- import_retries = 0
- import_rows = 810000
- oracle_passes = 18
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.1056528580" to 8388608 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.1613280838" to 8388608 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.1768436276" to 2097152 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.2064854610" to 4194304 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.2093834274" to 1048576 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.2105122858" to 4194304 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.2222317958" to 8388608 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.2526737516" to 4194304 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.267929764" to 8388608 bytes: No = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.3269856430" to 2105344 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.3292256260" to 8388608 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.457705562" to 4194304 bytes: No = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.647435288" to 4194304 bytes: No = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.865490332" to 2097152 bytes: No = 1
- oracle_query_errors = 15
- t_attempt_err_40P01 = 1
- t_noop_cancel = 2135
- t_noop_order = 5658
- t_noop_pay = 2024
- t_retries = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.103635154" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1072438458" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1075440756" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1118719280" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1130391400" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.117096406" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1203234558" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1283963514" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1418186408" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1438471350" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1460236384" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1477198278" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1538111456" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1540645152" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1683832856" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1696159688" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1789887448" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.180264452" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.187702498" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1888745186" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1897839438" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1918299668" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1960226282" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1981664912" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.199184716" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1999373496" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2044395844" to 2097152 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2052214132" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2058002952" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2160282328" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2216707690" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2237763998" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2243328528" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2313710146" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2340841616" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2461839208" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2473032716" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2504620134" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2509187786" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2561332242" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2592657878" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2602651952" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2630484718" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2653432252" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2691283736" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2794663840" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2852440954" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2942777162" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3037716364" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3066341454" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3079847044" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3101399856" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.312428008" to 2097152 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3239234990" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3255866724" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3277164662" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3304693730" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3369644420" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3525771134" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3598272234" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3637053590" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3663757150" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3677558152" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.369555316" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3720521174" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3742087696" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3805208082" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3816969586" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3830093338" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.4101854904" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.411371148" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.417646360" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.4216401050" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.427446242" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.485917016" to 12615680 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.488901516" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.534682754" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.601142380" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.683871758" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.752544324" to 12615680 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.768927956" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.799331386" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.846582432" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.877126192" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.970233480" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.998305918" to 4194304 bytes: No = 1
- tear_query_errors = 86
- tear_snapshots = 5
