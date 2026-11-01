# stask-m2 report: pg16

- run: 2026-11-01T05:46:37Z, harness commit 6afb539, **INVALID: LOAD NOT APPLIED**
- scale: 10000000 sims (43000200 rows), seed stask1; load: T=4 P=4 R=2, import always every 5s, hot set 200 at 30%; 120 s elapsed
- machine: 8 CPUs, load1 start 0.99 / max 14.04 / end 14.04, disk free 4.7 GB

## LOAD NOT APPLIED

**This run is INVALID.** The load it was configured to apply did not happen, so "all oracles held" below compares empty sets and proves nothing, and no number of this report may be quoted.

- r2_page: 20 successes, minimum 30 (-r 2, -min-r2)

## Oracles

| oracle | checks | violations | example |
|---|---|---|---|
| acked_txns_present_final | 1 | 0 |  |
| history_match_journal_final | 1 | 0 |  |
| import_history_match_journal_final | 1 | 0 |  |
| journal_seq_gapless_final | 1 | 0 |  |
| no_order_without_items | 19 | 0 |  |
| no_orphan_order_items | 4 | 0 |  |
| order_items_match_journal_final | 1 | 0 |  |
| order_total_equals_items | 19 | 0 |  |
| orders_match_journal_final | 1 | 0 |  |
| r2_pages_disjoint_frozen | 19 | 0 |  |
| r2_pages_prefix_consistent_frozen | 19 | 0 |  |
| reserved_stock_equals_open_items | 19 | 0 |  |
| stock_invariant_q13 | 19 | 0 |  |
| tear_no_order_without_items | 4 | 0 |  |
| tear_no_orphan_items | 4 | 0 |  |
| tear_order_total_equals_items | 4 | 0 |  |
| tear_q13_at_one_snapshot | 8 | 0 |  |
| tear_r2_page_repeatable_in_snapshot | 4 | 0 |  |
| tear_reserved_equals_open_items | 4 | 0 |  |

No oracle was violated, but the run is INVALID (load not applied).

## Latency and throughput per class (scale 10000000 sims, load as above, measured)

| class | n ok | errors | per s | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|---|
| import | 142 | map[] | 1.2 | 568.16 | 811.34 | 955.95 | 970.44 |
| r1_keyset | 45469 | map[] | 378.2 | 3.38 | 8.16 | 11.52 | 33.24 |
| r1_point | 46048 | map[] | 383.0 | 1.46 | 4.86 | 7.16 | 18.11 |
| r2_page | 20 | map[53100:65] | 0.2 | 8526.93 | 10785.16 | 11980.03 | 11980.03 |
| r_join | 7 | map[53100:4] | 0.1 | 4206.71 | 24052.57 | 24052.57 | 24052.57 |
| t_cancel | 6945 | map[] | 57.8 | 10.74 | 19.67 | 26.13 | 101.20 |
| t_order | 14017 | map[] | 116.6 | 12.15 | 22.16 | 28.87 | 188.76 |
| t_pay | 7071 | map[] | 58.8 | 10.69 | 19.53 | 25.41 | 97.38 |
| tear_tx | 3 | map[] | 0.0 | 19753.95 | 21165.86 | 21165.86 | 21165.86 |

## Error samples (up to 3 distinct messages per class, full text; first_s = seconds into the measured window)

| class | code | count | first_s | message |
|---|---|---|---|---|
| r2_page | 53100 | 1 | 47.9 | ERROR: could not resize shared memory segment "/PostgreSQL.1278781908" to 4194304 bytes: No space left on device (SQLSTATE 53100) |
| r2_page | 53100 | 1 | 48.1 | ERROR: could not resize shared memory segment "/PostgreSQL.1012338014" to 4194304 bytes: No space left on device (SQLSTATE 53100) |
| r2_page | 53100 | 1 | 48.2 | ERROR: could not resize shared memory segment "/PostgreSQL.4247386054" to 4194304 bytes: No space left on device (SQLSTATE 53100) |
| r_join | 53100 | 1 | 48.1 | ERROR: could not resize shared memory segment "/PostgreSQL.1843147492" to 4194304 bytes: No space left on device (SQLSTATE 53100) |
| r_join | 53100 | 1 | 48.7 | ERROR: could not resize shared memory segment "/PostgreSQL.4249969334" to 4194304 bytes: No space left on device (SQLSTATE 53100) |
| r_join | 53100 | 1 | 50.1 | ERROR: could not resize shared memory segment "/PostgreSQL.3743674268" to 4194304 bytes: No space left on device (SQLSTATE 53100) |

## Import shape

shape that ran: **batched (5000 rows per transaction, 20 ms pause)**; every import re-prices 1% of the sims (100000 rows) and writes history; each transaction is its own journal row.

import transactions (latency of ONE transaction): 142 ok, errors map[], p50 568 ms, p95 811 ms, p99 956 ms, max 970 ms; batches 142, rows 710000

## Counters

- import_batches = 142
- import_retries = 0
- import_rows = 710000
- oracle_passes = 19
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.1220139112" to 8388608 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.127157770" to 4194304 bytes: No = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.1586058886" to 8388608 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.1701745416" to 8388608 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.2256004806" to 4194304 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.2277333758" to 8388608 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.2708926570" to 4194304 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.2815563860" to 8388608 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.342967090" to 2105344 bytes: No = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.3585911996" to 4194304 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.3820756252" to 8388608 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.4164550038" to 2097152 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.4224600754" to 2105344 bytes: N = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.478561548" to 8388608 bytes: No = 1
- oracle_query_err:no_orphan_order_items: ERROR: could not resize shared memory segment "/PostgreSQL.47982562" to 4194304 bytes: No  = 1
- oracle_query_errors = 15
- t_noop_cancel = 2910
- t_noop_order = 7483
- t_noop_pay = 2932
- t_retries = 0
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1008035940" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.107113342" to 12615680 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.113368596" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1148337584" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.119187742" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1210236148" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1219405770" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1223780912" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1248276558" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1257613310" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1297347380" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1302804674" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1341643684" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1365632174" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1370638516" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.138724532" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1494202138" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1610160548" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1632125932" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1654570542" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1748909320" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1786672398" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1787853894" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1792075156" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1824238498" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1850189106" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1946658908" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1951194280" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1958785988" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.1992623800" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2001042632" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2052286058" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2080695558" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2128098392" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2166548536" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2195164894" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2258330580" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2331157872" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.236033002" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2368587538" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2417469036" to 8388608 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2454748232" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2540604582" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2575107566" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2619219444" to 6311936 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2643235308" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2782051050" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2847780776" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2858198778" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2875012390" to 6311936 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2906237958" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2926016948" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2931852878" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.2978029512" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3016950322" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3085672850" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3102395564" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3138306620" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3219135098" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3229886710" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3242182948" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3256360906" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3299746780" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3395410998" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3406597620" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3416965534" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3483079354" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3522834062" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.352915808" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.354812100" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3556159554" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3565655388" to 6311936 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3576615238" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3577689186" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3593522312" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3595838166" to 6311936 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3602867124" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3602871124" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3609518746" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3740063858" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3746130728" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3779539964" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.383131052" to 12615680 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3836892862" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3838590762" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3873415358" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.3944337248" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.4052550532" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.4068106118" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.410195182" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.4107652980" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.4121417050" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.4204656044" to 12615680 bytes:  = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.4225635248" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.4281118134" to 4194304 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.433448138" to 12615680 bytes: N = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.443065252" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.448575930" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.498566742" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.511358290" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.517637700" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.609932544" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.686081686" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.725361970" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.762930440" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.941566618" to 4194304 bytes: No = 1
- tear_query_err: ERROR: could not resize shared memory segment "/PostgreSQL.993029644" to 4194304 bytes: No = 1
- tear_query_errors = 107
- tear_snapshots = 4
