# 12 — Kế hoạch hoàn thiện bản Go

Kế hoạch này gom **mọi** giới hạn, mục "chưa hỗ trợ" và "việc tiếp theo" trong docs/03–11 vào một chỗ. Mỗi mục có hướng xử lý, giai đoạn, phụ thuộc và tiêu chí xong. Mục tiêu là qua Cổng 1 và Cổng 2 của docs/11.

Cách lập (2026-10-29): đọc lại docs/03 §9, 04 §5–7, 05 §3, 06 §3–4, 07 §5, 08 §7, 10 §5 và §7, 11 §2.2, rồi đối chiếu với mã nguồn hiện tại. Mục nào trong tài liệu đã cũ (đã làm xong) được ghi ở §9 để không bị tính lại.

**Quy tắc chung cho mọi mục:**
1. Tìm hiểu cách các hệ thật đã giải trước khi thiết kế, và ghi link vào tài liệu của mục đó.
2. Có test đơn vị, test `-race`, và một phép thử nghiệm thu thêm vào docs/09 §10.
3. Đo trước và sau theo docs/11 §5: ghim CPU, ít nhất 3 lượt, ghi cả CPU của client.
4. Không làm xấu loại thao tác nào. Nếu có thì ghi rõ và có hướng xử lý.
5. Mục nào đổi định dạng trên đĩa thì cập nhật docs/09 và giữ khả năng đọc dữ liệu cũ.
6. **Plan–target–actual (docs/14):** mỗi task khi xong phải điền kế hoạch, mục tiêu và thực tế (bằng chứng tái lập được: test, probe `cmd/claimcheck`, lệnh đo) vào docs/14, và nêu chênh lệch với docs. Task không có bằng chứng thì không được ghi "xong". Build bằng `go build -o bin-main/ ./cmd/...` trước khi đo hay probe (không dùng `bin/` cũ).

Cỡ việc: **S** vài giờ, **M** 1–2 ngày, **L** 3–5 ngày, **XL** trên một tuần.

## 0. Ba vấn đề mới phát hiện khi rà lại

Ba vấn đề này chưa có trong danh sách nào trước đây:

| # | Vấn đề | Bằng chứng | Vì sao quan trọng |
|---|---|---|---|
| N1 | **Khi quá tải, đọc không được ưu tiên.** Ở tải 2x, đọc chỉ được phục vụ 60% (23,9k/40k, chờ 2,9 s), trong khi range, tx và trang đều được phục vụ đủ. | `results/sweep-adm-x1.5.txt`, `-x2.txt` | Tiêu chí cân bằng yêu cầu **đọc được ưu tiên tuyệt đối**. Hiện điều này chưa đạt. |
| N2 | **Kiểu dữ liệu chưa trung thực với PG:** `timestamp` chỉ tới mili giây (PG tới micro giây), `numeric` lưu dạng float64, `uuid`/`json`/`jsonb` lưu như text, chưa có `bytea` và cột mảng. | `internal/schema` (TTime là unix ms), `internal/sql/parse.go` | Đổi định dạng dòng, nên phải làm **trước** khi port. Tiền tệ và ORM cần đúng kiểu. |
| N3 | **So sánh WL-1 với PG chưa cùng client.** PG đo bằng pgbench, STASK đo bằng client HTTP/JSON. Client HTTP còn tốn 2,5 core ở 40k ops/s. | `results/redis-6x2`, docs/11 §5 | Kết luận "nhanh hơn PG" ở WL-1 chưa chặt chẽ. |

Ngoài ra, docs/11 §2.2 ghi "trigger **và stored procedure**: Có", mâu thuẫn với docs/10 §5 (stored procedure: không làm). Kế hoạch này theo docs/10: **trigger thân SQL thì làm, ngôn ngữ thủ tục thì không** (xem P7). docs/11 đã được sửa cho khớp.

## P0. Đo lường cho chặt (làm ngay, trước mọi tính năng)

| # | Việc | Cỡ | Tiêu chí xong |
|---|---|---|---|
| P0.1 | ~~Phép so với Redis~~ **xong** (docs/04 §9, §10.1) | S | — |
| P0.2 | **Xong** (docs/04 §10.1). Thêm chế độ `-pg <url>` vào WL-1 của `staskbench` (pgx, prepared statement), để đo **STASK qua PG wire và PG16 bằng cùng một client** (N3) | M | WL-1 có ba cột cùng client: STASK, PG16, Redis |
| P0.3 | **Xong.** Mọi công cụ đo tự ghi CPU của client và cảnh báo khi vượt 70% | S | Có trong JSON kết quả và trong bản tóm tắt |
| P0.4 | **Xong** (docs/04 §10.2–10.4; `stress` chưa chạy lại). Đo lại với CPU đã ghim và hai bản sửa hôm nay: `latcompare`, `bulkcompare`, `stress`, `pagesweep` (STASK và PG) | M | docs/05 §6–7 và docs/06 có số mới |
| P0.5 | **Còn nghẽn, nặng hơn: đọc p99 558 ms ở lượt 1, do chờ trong hàng đợi** (docs/04 §10.6). Đang profile. Đo lại tải hỗn hợp đầy đủ ở 100M, để xem đợt nghẽn khoảng 100 ms ở lượt 1 (docs/08 §7 #1) còn không | S | Còn thì profile và sửa; hết thì đóng mục |
| P0.6 | Bài đo GC dưới tải cập nhật nặng, gồm cả segment đã nén (docs/07 §5 #4) | M | Dung lượng, CPU GC và độ trễ ổn định sau 30 phút |
| P0.7 | **Xong ở 10 GB** (1,05–2,42 s, docs/04 §10.5); còn nợ số nguội ở 100M. RTO khởi động nguội, page cache trống (docs/04 §6) | S | Ghi số ở 10 GB và ở 100M |
| P0.8 | **Ưu tiên đọc khi quá tải** (N1): xếp hàng có ưu tiên, hoặc lane riêng, trong admission. Cách chọn tham khảo CockroachDB admission control | L | Ở tải 2x, đọc được phục vụ ≥ 99% với p99 < 50 ms; các loại khác xuống cấp có kiểm soát; có báo cáo tỉ lệ phục vụ theo từng loại |

**Trạng thái P0.8 (nhánh `admission`, 2026-10-29): đã cài và có test mô phỏng; chưa có số đo thật ở 2x.** Thiết kế ở docs/08 §Admission theo ưu tiên (`internal/sql/admission.go`): đọc không bao giờ xếp hàng chờ slot (trước đây đọc dùng chung slot với ghi, chỉ được xếp trước); ghi và lệnh nặng (range, quét, ghi nhiều dòng) mỗi loại có hồ slot riêng và chỉ bị trễ, không bị bỏ. Test `TestAdmissionReadPriorityUnderOverload` mô phỏng 4 core bị 96 client ghi/nặng/tx làm bão hoà: đọc đúng hạn 50 ms đạt 100% (p99 5 ms) so với 0% khi tắt admission (p99 115 ms), ghi và nặng vẫn chạy. Đây là mô hình, không phải số đo máy thật. `/v1/status` có khối `admission` với tỉ lệ phục vụ theo loại. **Chạy lại ở 2x:** `cd ~/stask && SCALES="1.5 2" bash scripts/slotsweep.sh "-admission -admission-slots 0 -heavy-workers -1"` (xem tỉ lệ `served read`), rồi so với `results/sweep-adm-x2.txt`. Lưu ý: nếu nút thắt của đọc là CPU chứ không phải hàng đợi thì còn cần giới hạn số slot ghi nhỏ hơn số core (`-admission-slots`).

## P1. Kiểu dữ liệu và schema (đổi định dạng, làm đầu tiên)

| # | Việc | Cỡ | Phụ thuộc | Tiêu chí xong |
|---|---|---|---|---|
| P1.1 | **Kiểu dữ liệu đúng như PG** (N2): timestamp tới micro giây, `numeric` chính xác (decimal), `uuid` 16 byte, `bytea`, `json`/`jsonb` có kiểm tra cú pháp, cột mảng. Có đường di trú cho dữ liệu cũ | XL | — | `sqlcorpus` có thêm câu cho từng kiểu, đối chiếu PG cho đúng 100%; pgoutput và CDC gửi đúng OID |
| P1.2 | **Đã làm 2026-10-29 trừ ALTER COLUMN TYPE** (docs/05 §11, docs/09 §5): `ALTER TABLE ADD/DROP/RENAME COLUMN`, `ALTER COLUMN TYPE/SET DEFAULT/SET NOT NULL`, `RENAME TABLE`. Dòng cũ đọc theo phiên bản schema, không phải viết lại cả bảng. **Thiết kế đã chọn (2026-10-29):** giữ lịch sử layout của bảng, mỗi layout có LSN bắt đầu. Dòng được giải mã theo layout có hiệu lực tại LSN commit của version chứa nó (LSN này đã có sẵn cho MVCC). Như vậy định dạng dòng không đổi và không tốn thêm byte nào. Đây là ý của [`attmissingval` của PG 11](https://www.brandur.org/postgres-default) và [row version của MySQL INSTANT](https://dev.mysql.com/blog-archive/mysql-8-0-instant-add-and-drop-columns/), nhưng lấy phiên bản từ LSN thay vì ghi vào header dòng. DROP COLUMN chỉ đánh dấu cột (như `attisdropped`). Cần đổi 13 chỗ gọi Decode để truyền LSN | L | P1.1 | Test với dữ liệu cũ; view cột, search và CDC (thông điệp Relation) theo đúng schema mới |
| P1.3 | ~~`DEFAULT` (kể cả `now()`, `gen_random_uuid()`), `CHECK`, `NOT NULL`~~ (**đã làm 2026-10-29**; `NOT NULL` có từ trước) | M | P1.2 | SQLSTATE 23514 và 23502 như PG |
| P1.4 | ~~Sequence, `SERIAL`/`IDENTITY`, `nextval`/`currval`/`setval`~~ (**đã làm 2026-10-29**, docs/05 §2). Trạng thái bền, cấp phát theo lô như cache của PG | M | — | Không trùng số sau crash (có test chaos) |
| P1.5 | ~~`DROP TABLE`, `TRUNCATE`~~ (**đã làm 2026-10-29**, docs/05 §2, docs/09 §5, docs/10 §6), ~~`CREATE VIEW` (không materialized)~~ (**đã làm 2026-10-29**, docs/05 §16; còn liệt kê trong `pg_class`/`information_schema` ở P4.1), `COPY FROM/TO STDIN/STDOUT` (text và csv) | M | — | psql `\copy` chạy được; COPY 10M dòng ít nhất nhanh bằng INSERT theo lô |

## P2. Index phụ và đường truy cập

| # | Việc | Cỡ | Phụ thuộc | Tiêu chí xong |
|---|---|---|---|---|
| P2.1 | **Index phụ**: `CREATE [UNIQUE] INDEX` trên một hoặc nhiều cột, duy trì trong đường ghi, lưu trong checkpoint, dựng lại khi khởi động. Có MVCC (index trỏ vào khoá chính, kiểm tra visibility) | XL | P1.1 | Planner dùng index cho `=`, `IN`, khoảng, `ORDER BY ... LIMIT`; đo chi phí ghi khi có 1, 3 và 5 index |
| P2.2 | `UNIQUE` ngoài khoá chính (dựa trên P2.1) và khoá ngoại (`REFERENCES`, `ON DELETE CASCADE/RESTRICT/SET NULL`) | L | P2.1 | SQLSTATE 23505 và 23503; test có giao dịch đồng thời |
| P2.3 | **Serializable cho cả quét** (hiện quét bị từ chối): SSI theo khoảng của index, như PG | L | P2.1 | Bộ test bất thường write skew của PG qua; WL-5 ở mức serializable |
| P2.4 | Bộ thực thi nhiều bảng tràn xuống đĩa khi vượt bộ nhớ (hash join, sort, aggregate) | L | — | Join 2 bảng 10M dòng với giới hạn bộ nhớ 1 GB: chạy xong, không OOM |

## P3. Dữ liệu lớn hơn RAM, lưu trữ

| # | Việc | Cỡ | Phụ thuộc | Tiêu chí xong |
|---|---|---|---|---|
| P3.1 | **Index chính trên đĩa** (B+tree có buffer pool, hoặc LSM). Chọn sau khi tìm hiểu và đo thử cả hai | XL | P2.1 (dùng chung cấu trúc) | Dữ liệu gấp 3 lần RAM; đọc điểm khi trúng cache vẫn gần mức hiện tại; tiêu chí cân bằng giữ được |
| P3.2 | **View cột lưu trên đĩa, nén theo cột** (Delta/DoubleDelta cho id và thời gian, ZSTD cho số thực và văn bản). Giải quyết cùng lúc: dựng lại mất 182 s (docs/08 §7 #3), khoảng cách nén 2,6 lần so với ClickHouse (docs/07 §5 #2), và cột số thực chưa nén | XL | P1.1 | Khởi động 100M có view sẵn sàng dưới 20 s; trên đĩa không quá 1,5 lần ClickHouse ZSTD; aggregate không chậm đi |
| P3.3 | Checkpoint tăng dần, chỉ ghi phần đổi; GC phân tầng nóng/lạnh; bảng remap bền qua khởi động lại (docs/03 §9) | L | P3.1 | Checkpoint ở 100M dưới 5 s khi tải; GC không cần checkpoint đầy đủ mỗi vòng |
| P3.4 | Truy vấn time-series, search và `AS OF` từ SQL (hiện chỉ có qua HTTP) | M | — | Có cú pháp, có test, có trong sqlcorpus |
| P3.5 | `ARRAY JOIN`, `FINAL`, `SAMPLE` của ClickHouse (hiện được chấp nhận nhưng bỏ qua) | M | P1.1 (cột mảng) | Đối chiếu ClickHouse đúng 100% |

## P4. Catalog, công cụ và ORM

| # | Việc | Cỡ | Phụ thuộc | Tiêu chí xong |
|---|---|---|---|---|
| P4.1 | **Đã làm 2026-10-29, chưa thử với psql/DBeaver/JDBC thật** (docs/05 §17): `pg_catalog` (`pg_class`, `pg_attribute` kể cả cột đã xoá, `pg_type`, `pg_index`, `pg_namespace`, `pg_constraint`, `pg_attrdef`, `pg_depend`…) và `information_schema`, cùng các hàm `pg_get_expr`, `format_type`, `regclass`… Các câu của psql 16 `\d`, `\dt`, `\di`, `\ds`, `\d tbl`, `\l`, pgjdbc `getTables/getColumns/getPrimaryKeys/getIndexInfo` và DBeaver chạy qua pgx. **Còn lại:** thử với psql, JDBC, DBeaver thật; ghi gói tin của chúng để đối chiếu; index phụ, khoá ngoại, view, `COMMENT ON` trong catalog (cùng với P1/P2.1); `regclass` in tên | L | P1, P2.1 | psql `\d`, `\dt`, `\di` chạy; DBeaver duyệt được schema |
| P4.2 | **Nghiệm thu bằng ứng dụng thật**: một ứng dụng Jmix 2 (Hibernate/EclipseLink + Liquibase) chạy với STASK làm datastore chính | L | P4.1, P1, P2.2 | Liquibase tạo schema; CRUD qua UI; bộ test của ứng dụng qua |
| P4.3 | Debezium chạy thật: ghi lại các truy vấn catalog nó gửi, rồi bổ sung | M | P4.1 | Debezium Server stream 1M thay đổi, không mất, đúng thứ tự |
| P4.4 | Xuất snapshot khi tạo slot (`USE_SNAPSHOT`, `SET TRANSACTION SNAPSHOT`, `COPY ... TO STDOUT`) cho `copy_data = true` | M | P1.5 | `CREATE SUBSCRIPTION` mặc định (copy_data) chạy; Debezium snapshot ban đầu đúng |

## P9. Bảo mật, dữ liệu, bộ nhớ, tài khoản, kết nối (rà soát 2026-10-29)

Rà theo code hiện tại. Các mục **in đậm** làm trước P4.2 (chạy ứng dụng Jmix thật), vì chúng là lỗ hổng chứ không phải tính năng thiếu.

| # | Hiện trạng (đã kiểm trong code) | Việc | Cỡ | Xử lý |
|---|---|---|---|---|
| ~~**S1**~~ | Cổng nhân bản (`-repl-addr`) **không xác thực, không mã hoá**: ai kết nối được là stream được toàn bộ dữ liệu | TLS + xác thực replica (mTLS hoặc secret dùng chung, so sánh constant-time) | M | **Xong (2026-10-29)**: TLS verify-full, secret chứng minh bằng HMAC challenge-response hai chiều (có channel binding), mTLS tuỳ chọn, `-repl-require-secure`; không xác thực thì từ chối bind địa chỉ không phải loopback trừ khi `-repl-insecure`. Xem docs/06_cluster.md §2 "Bảo mật cổng nhân bản" |
| ~~**S2**~~ | API HTTP (`-addr`) **không xác thực**: đọc/ghi mọi bảng. Mặc định chỉ nghe 127.0.0.1 nên hiện an toàn trên một máy | Token bearer hoặc mTLS, TLS. Từ chối nghe địa chỉ không phải loopback nếu chưa bật xác thực | M | **Xong (2026-10-29)**: TLS + mTLS, token bearer đầy đủ/chỉ đọc (file đọc lại khi đổi hoặc SIGHUP, so sánh constant-time), Basic với `-pg-password` tuỳ chọn; không có xác thực thì từ chối bind ngoài loopback trừ `-http-insecure`; `/v1/status` cần token, `/v1/health` mở. Xem `docs/13_http_security.md` |
| ~~S3~~ | `-pprof` và endpoint lỗi giả lập không có xác thực | Chỉ cho bind loopback; cảnh báo khi khởi động | S | **Xong (2026-10-29)**: `-pprof` chỉ loopback trừ `-pprof-insecure`, dùng chung xác thực của API; `/v1/admin/fault` cần token đầy đủ; cảnh báo khi khởi động. Xem `docs/13_http_security.md` |
| S4 | Một mật khẩu dùng chung cho mọi kết nối PG; không có user/role; đổi mật khẩu phải restart | `CREATE/ALTER ROLE ... PASSWORD` (verifier SCRAM lưu trong catalog), `GRANT/REVOKE` (P5.2), luật kiểu `pg_hba.conf` (IP, user, db, phương thức) | L | P5.2 mở rộng |
| S5 | Không giới hạn số lần đăng nhập sai; chưa có SASLprep | Trễ tăng dần và khoá tạm theo IP/user; SASLprep (RFC 4013) | S | Cùng S4 |
| S6 | Log và checkpoint lưu dạng rõ trên đĩa | Mã hoá ở tầng đĩa (LUKS/dm-crypt), giống cách khuyến nghị cho PG (PG cũng không có TDE). Backup (P5.4) mã hoá | — | Không làm TDE riêng; ghi hướng dẫn vận hành |
| S7 | Không có audit log | Ghi đăng nhập, DDL, GRANT ra file riêng | S | Sau S4 |
| D1 | Backup nóng, PITR: đã có mã (docs/05 §14), còn thử end-to-end | = P5.4 | L | P5 |
| D2 | Bản ghi có CRC nhưng không quét định kỳ | Quét nền CRC của segment và checkpoint (scrub), báo hỏng sớm | S | P5 |
| D3 | Dòng đã xoá còn trong segment cũ tới khi GC qua horizon (mặc định giữ lịch sử 1 h) | Ghi rõ thời hạn xoá thật; lệnh ép GC cho yêu cầu xoá dữ liệu cá nhân | S | P5 |
| ~~**M1**~~ | Truy vấn nhiều bảng đưa hết vào RAM, không có ngân sách: một truy vấn lớn có thể làm cả tiến trình OOM | Ngân sách bộ nhớ theo truy vấn và theo node, vượt thì trả `53200` thay vì chết (kiểu [`--max-sql-memory` của CockroachDB](https://www.netdata.cloud/guides/cockroachdb/cockroachdb-sql-memory-budget-53200/)); spill ra đĩa ở P2 | M | **Xong (2026-10-29)**: `-query-mem-mb`, `-sql-mem-mb`, lỗi `53200`; docs/05 §12 |
| ~~M2~~ | ~~Không huỷ được câu lệnh (CancelRequest bị bỏ qua); không có `statement_timeout`~~ | = P5.3 | M | **Đã làm 2026-10-29** (docs/05 §11): CancelRequest kiểm pid + khoá ngẫu nhiên (so constant-time), `statement_timeout` (SET và `-statement-timeout`), cùng trả `57014` |
| ~~M3~~ | ~~Giao dịch mở **ghim snapshot**; horizon của GC = snapshot cũ nhất (`gc.go`). Một session bỏ quên trong `BEGIN` sẽ chặn thu hồi log mãi, đĩa đầy dần~~ | `idle_in_transaction_session_timeout`, `idle_session_timeout`; trần tuổi snapshot (quá thì `snapshot too old`); hiện snapshot ghim lâu nhất ở `/v1/status` | S | **Đã làm 2026-10-29** (docs/05 §11): hai timeout (FATAL `25P03`/`57P05`, bỏ ghim snapshot), `-max-snapshot-age` mặc định 24h (như `tidb_gc_max_wait_time`), `oldest_snapshot_age_ms` và `snapshot_cut_lsn` trong khối `gc` |
| M4 | Heap Go ~8,5 GB ở 100M dòng; nghi chu kỳ GC của Go gây nghẽn (P0.5, đang đo bằng `gctrace`) | Index không chứa con trỏ (arena/offset) để GC khỏi quét | L | Theo kết quả P0.5 |
| M5 | Chưa đo bộ nhớ của mỗi kết nối và mỗi prepared statement | Đo 10k kết nối rảnh; giới hạn số prepared statement mỗi session | S | Làm ngay (chỉ đo) |
| C1 | Mỗi kết nối là một goroutine (rẻ hơn tiến trình của PG), trần `-pg-max-conns` 100k, nên thường không cần PgBouncer. Chưa thử pool phía client của Jmix (HikariCP) | Test HikariCP: `isValid`, reset session, `DISCARD ALL`, `SET` | S | Cùng P4.2 |
| C2 | Chưa thử PgBouncer ở transaction mode (prepared statement ở mức giao thức) | Test tương thích, ghi kết quả | S | Cùng P4.2 |
| C3 | Không có giới hạn kết nối theo user/db; khi đầy, admin cũng không vào được | `CONNECTION LIMIT` theo role; chừa sẵn slot cho admin (như `superuser_reserved_connections`) | S | Cùng S4 |
| ~~C4~~ | ~~Không phát hiện client chết khi mạng rớt~~ | TCP keepalive, `tcp_user_timeout` | S | **Đã làm 2026-10-29**: `-pg-tcp-keepalives-idle/interval/count` (mặc định 1m/10s/6), `-pg-tcp-user-timeout` (Linux) |

## P5. Bảo mật và vận hành

| # | Việc | Cỡ | Tiêu chí xong |
|---|---|---|---|
| P5.1 | ~~TLS (PG wire), SCRAM-SHA-256~~ **đã làm 2026-10-29** (docs/05 §10); còn TLS cho HTTP | M | psql `sslmode=verify-full` và JDBC kết nối được |
| P5.2 | Vai trò, `GRANT`/`REVOKE` ở mức bảng | L | SQLSTATE 42501; test quyền cho cả walsender và CDC |
| P5.3 | ~~CancelRequest, `statement_timeout`, `idle_in_transaction_session_timeout`~~ **đã làm 2026-10-29** (docs/05 §11): aggregate quét toàn bảng bị huỷ sau 6–35 ms qua pgx (8 worker chiếm hết CPU); thêm `idle_session_timeout`, trần tuổi snapshot, TCP keepalive | M | Huỷ được một aggregate dài trong vòng 100 ms |
| P5.4 | Backup nóng và PITR (dùng chính log). **Đã viết 2026-10-29, docs/05 §14; test đơn vị qua, chưa thử end-to-end qua mạng/TLS và tải lớn** | L | Khôi phục tới một thời điểm bất kỳ; dữ liệu trùng khớp |
| P5.5 | Metrics kiểu Prometheus (`/metrics`), log truy vấn chậm, `pg_stat_activity` tối thiểu | M | Có dashboard mẫu; không tốn quá 1% CPU |

## P6. Cluster

| # | Việc | Cỡ | Tiêu chí xong |
|---|---|---|---|
| P6.1 | A2: đọc-ghi-của-mình qua replica (client mang LSN, replica chờ tới đó) và bộ điều hướng tự động | M | Đọc ngay sau khi ghi, qua replica, luôn thấy bản ghi đó |
| P6.2 | B: xác nhận ghi theo đa số (Raft), failover, lease cho leader | XL | Chaos: giết leader và chia mạng, 0 ghi đã xác nhận bị mất; có số đo thời gian chuyển leader |
| P6.3 | Slot, publication và replica identity đi qua log, để sống sót qua failover (docs/10 §7) | M | Subscriber PG tiếp tục sau failover, không mất, không trùng |
| P6.4 | Đo cluster trên **các máy riêng** (hiện chỉ có 1 VM chia core, docs/06 §4) | M | **Cần thêm VM**: phần này phụ thuộc vào bạn |
| P6.5 | C: phân mảnh, giao dịch xuyên shard | XL | **Sau quyết định ngôn ngữ** (docs/11): làm trên bản thắng |

## P7. Trigger

| # | Việc | Cỡ | Tiêu chí xong |
|---|---|---|---|
| P7.1 | **Đã làm 2026-10-29** (docs/05 §18, docs/10 §5): trigger thân SQL kiểu SQLite: `BEFORE/AFTER INSERT/UPDATE/DELETE`, `FOR EACH ROW`, `NEW`/`OLD`, `WHEN`. Chạy trong giao dịch ghi (rollback cùng nhau), chống đệ quy (sâu tối đa 16 tầng, rồi 54001), định nghĩa bền vững như `CREATE VIEW`, không chạy lại trên replica. Thêm hai mở rộng để không cần ngôn ngữ thủ tục: `SET NEW.col = expr` trong BEFORE, và `SELECT RAISE(ABORT, 'msg') [WHERE ...]`. **Đo:** bảng không có trigger: 10,9 / 10,9 / 11,9 µs (trước) so với 11,0 / 11,3 / 11,8 µs (sau) cho insert+update+delete, cùng số cấp phát (36), tức không chậm đi; có 3 trigger mỗi trigger ghi 1 dòng: ~27–28 µs (thêm ~5,5 µs mỗi lần chạy thân). **Chưa làm:** `UPDATE OF cols`, trigger mức câu lệnh, `INSTEAD OF`, `REFERENCING`, `RAISE(IGNORE)`, `pg_get_triggerdef` (vẫn trả NULL), `information_schema.triggers` | L | Có test; khi bảng không có trigger thì ghi không chậm đi |
| P7.2 | Cú pháp `CREATE FUNCTION ... RETURNS trigger LANGUAGE plpgsql` tối thiểu, đủ cho dạng trigger mà Liquibase và Jmix sinh ra (nếu P4.2 cần). **Chưa làm (2026-10-29):** P4.2 chưa gặp ca nào cần, và `CREATE TRIGGER ... EXECUTE FUNCTION f()` trả lỗi 0A000 rõ ràng. Khi cần, chỉ hỗ trợ thân hàm dạng `BEGIN NEW.col := expr; RETURN NEW; END` dịch sang `SET NEW.col`, còn lại từ chối | M | Chỉ làm nếu P4.2 gặp |
| — | Stored procedure và ngôn ngữ thủ tục đầy đủ | — | **Không làm** (docs/10 §5) |

## P8. Cổng 2 và quyết định port

| # | Việc | Phụ thuộc |
|---|---|---|
| P8.1 | Chạy toàn bộ bộ tải ở docs/11 §5 trên bản Go hoàn chỉnh, lập bảng chi phí runtime Go | P0–P7 |
| P8.2 | Thử `simd/archsimd` (Go 1.26) cho quét cột trước khi tính quét cột là chi phí của ngôn ngữ | P3.2 |
| P8.3 | Báo cáo Cổng 2, quyết định có port hay không | P8.1, P8.2 |

## Thứ tự đề xuất

```
P0 (đo lường, ưu tiên đọc)   ──►  P1 (kiểu, schema)  ──►  P2 (index phụ, ràng buộc, SSI)
                                                             │
                     ┌───────────────────────────────────────┤
                     ▼                                       ▼
        P3 (vượt RAM, view cột trên đĩa)        P4 (catalog, Jmix, Debezium)
                     │                                       │
                     └──────► P5 (bảo mật, vận hành) ◄───────┘
                                       │
                          P6.1–6.3 (cluster A2, B)   P7 (trigger)
                                       │
                                  P8 (Cổng 2)
```

Lý do của thứ tự:
- **P0 trước**, vì mọi so sánh sau dựa vào nó. P0.8 (ưu tiên đọc) là vi phạm tiêu chí cân bằng đang tồn tại, không phải tính năng mới.
- **P1 và P2 trước P3**, vì kiểu dữ liệu và index phụ quyết định định dạng mà index trên đĩa (P3.1) phải lưu.
- **P4 cần P1 và P2**, vì catalog phải mô tả ràng buộc và index thật.
- **P6.2 (Raft) đặt sau P3**, vì nó đổi định dạng log. Làm sau khi định dạng dòng và index đã ổn định thì tránh phải đổi hai lần.

## 9. Mục trong tài liệu cũ đã xong (không tính lại)

| Mục | Ở đâu | Trạng thái |
|---|---|---|
| "Không có SQL, không PG wire" | docs/04 §6 | Đã có (docs/05) |
| "Một bảng mỗi truy vấn, không JOIN/subquery" | docs/03 §9 | Đã có bộ thực thi tổng quát (docs/05 §9) |
| "HTTP/JSON là nút thắt của WL-1" | docs/04 §5.1 | Đã sửa (docs/04 §8.1). Giờ nút thắt là client đo, xem N3 |
| "Một node, không replication" | docs/03 §9, 04 §6 | Giai đoạn A xong; B ở P6.2 |
| `pg_catalog` | docs/05 §3, §14 | Đã có phần đọc schema hiện có; phần còn lại ở P4.1 |
| Aggregate làm thao tác điểm phải chờ | docs/08 §7 #4 | Đã sửa (docs/08 §8.5) |
| `/v1/status` treo khi dựng view | docs/08 §8.4 | Đã sửa |
| Thu gom log | docs/04 §6 | Đã có |
| File cấu hình và tự nhận diện phần cứng | tiêu chí cân bằng | Đã có (`-config`, `stasktune`) |

## 10. Mục không làm (có lý do)

| Mục | Lý do | Nguồn |
|---|---|---|
| Stored procedure, ngôn ngữ thủ tục | Khó test và vận hành; CDC, NOTIFY và outbox đã thay được | docs/10 §5 |
| Stream giao dịch dở dang | Giao thức cho phép gửi nguyên giao dịch | docs/10 §7 |
| DDL thành sự kiện CDC | PG cũng không làm; thông điệp Relation đã đủ | docs/10 §7 |
| NOTIFY bền | Giống PG; cần bền thì dùng CDC | docs/10 §7 |
| Publication không theo giao dịch | Chỉ khác ở ca BEGIN rồi ROLLBACK | docs/10 §7 |
| WAF thiết bị cao khi tải thưa | Cái giá của RPO = 0; PG chịu tương tự | docs/04 §5.4 |
| Giao dịch HTTP giữ trạng thái ở server | Đường chính là PG wire; HTTP giữ như hiện tại | docs/03 §9 |

> **Ghi chú đo đạc (2026-10-29):** từ khi có DROP TABLE thật, pha nạp của staskmix (`DROP TABLE IF EXISTS orders`) xoá mất bảng columnar mà script tạo trước (aggbench `-exec`), nên các phép đo aggregate chạy sau đó đi qua đường quét dòng (100M: 14 s thay vì 0,18 s). Đã sửa: staskmix chỉ DROP khi bảng có dữ liệu. Các kết quả aggregate đo từ lúc thêm DROP TABLE tới lúc sửa (`results/scale/p05-gc`, `p05b`) không hợp lệ. Trong `p05b`, mix-1 **không** nghẽn (read p99 51 ms), còn ở `p05-gc` lượt chạy không có aggregate vẫn nghẽn (357 ms), nên P0.5 chưa có kết luận.
