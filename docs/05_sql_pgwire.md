# 05 — SQL và giao thức PostgreSQL

STASK nhận kết nối bằng giao thức PostgreSQL v3, nên driver PostgreSQL có sẵn kết nối được mà không cần viết driver riêng: psql/libpq, pgx (Go), JDBC, psycopg. Ngôn ngữ truy vấn là một **tập con của SQL theo cú pháp PostgreSQL**, chạy trực tiếp trên engine giao dịch: cùng MVCC, cùng group commit, cùng RPO=0.

Mã nguồn:
- `internal/sql`: lexer, parser, bộ tính biểu thức, bộ thực thi, suy luận kiểu.
- `internal/pgwire`: server giao thức.
- `cmd/staskstress`: công cụ stress theo số kết nối.

## 1. Chạy và kết nối

```sh
staskd -dir /data/stask -pg-addr 0.0.0.0:5432 -pg-password 'bí-mật'
psql "host=stask-host port=5432 user=app dbname=stask"
```

- `-pg-addr` mặc định là `127.0.0.1:17432`; để rỗng thì tắt.
- `-pg-password`: nếu đặt, server yêu cầu mật khẩu (mặc định dạng cleartext; `-pg-auth scram` để dùng SCRAM-SHA-256). TLS bật bằng `-pg-tls-cert`/`-pg-tls-key`, xem §10.
- Tên user và database được chấp nhận nhưng chưa có phân quyền.

URL dùng được với các driver:
- JDBC: `jdbc:postgresql://host:17432/stask?sslmode=disable`
- pgx: `postgres://app@host:17432/stask?sslmode=disable`

## 2. Những gì đã hỗ trợ

**Kiểu dữ liệu.** Các tên kiểu PostgreSQL được ánh xạ sang 5 kiểu lưu trữ:

| Kiểu SQL | Kiểu lưu | OID trả về |
|---|---|---|
| `int`, `integer`, `bigint`, `smallint`, `serial` | int64 | int8 (20) |
| `real`, `float`, `double precision`, `numeric` | float64 | float8 (701) |
| `text`, `varchar(n)`, `char(n)` | chuỗi | text (25) |
| `boolean` | bool | bool (16) |
| `timestamp`, `timestamptz`, `date` | thời gian, độ phân giải ms, UTC | timestamptz (1184) |

`numeric` hiện được lưu dạng float64, nên **chưa dùng được cho tiền tệ cần chính xác tuyệt đối**. Với tiền, hãy lưu số nguyên theo đơn vị nhỏ nhất, ví dụ đồng.

**Câu lệnh:**
- `CREATE TABLE [IF NOT EXISTS]` với đúng một khoá chính, `NOT NULL`, và `WITH (...)` để bật các view dẫn xuất: `columnar`, `search='cột1,cột2'`, `append_only`, `time_column`, `series_column`.
- **Sequence và giá trị mặc định** (thêm 2026-10-29), theo [PG 16](https://www.postgresql.org/docs/16/sql-createsequence.html):
  - `CREATE SEQUENCE [IF NOT EXISTS] s [AS smallint|integer|bigint] [INCREMENT [BY] n] [MINVALUE n | NO MINVALUE] [MAXVALUE n | NO MAXVALUE] [START [WITH] n] [CACHE n] [[NO] CYCLE]`, `ALTER SEQUENCE [IF EXISTS] s ... [RESTART [WITH n]]`, `DROP SEQUENCE [IF EXISTS] s [, ...]`.
  - Hàm `nextval`, `currval`, `lastval`, `setval(s, v [, is_called])`. Tên viết như regclass: `'s'`, `'public.s'`, `'s'::regclass`. Đọc được `SELECT last_value, log_cnt, is_called FROM s` (cách Jmix đọc giá trị hiện tại) và view `pg_sequences`.
  - `nextval` không thuộc giao dịch: số đã cấp không bao giờ cấp lại, kể cả khi rollback. Sau crash có thể nhảy số, như PG.
  - Trạng thái bền theo cách của PG ([SEQ_LOG_VALS](https://github.com/postgres/postgres/blob/REL_16_STABLE/src/backend/commands/sequence.c)): mỗi lần ghi log dành trước 32 giá trị, nên 31/32 lần `nextval` không ghi gì. **Khác PG:** sau cả restart sạch, sequence tiếp tục sau mốc đã dành trước (PG chỉ nhảy số khi crash). Mỗi sequence là một dòng trong bảng hệ thống ẩn `__stask_sequences`, nên được ghi log, checkpoint, khôi phục và nhân bản như dữ liệu thường, không cần cơ chế riêng. CDC không phát dòng của bảng này.
  - Cột: `DEFAULT expr` (hằng, `now()`, `gen_random_uuid()`, `nextval(...)`; không được tham chiếu cột hay subquery, như PG), `serial`/`bigserial`/`smallserial`, `GENERATED ALWAYS | BY DEFAULT AS IDENTITY [(tuỳ chọn sequence)]`. Sequence của cột tên `bảng_cột_seq`, bị drop cùng bảng và đặt lại bởi `TRUNCATE ... RESTART IDENTITY`. `DROP SEQUENCE` một sequence đang thuộc cột trả `2BP01`.
  - `INSERT ... DEFAULT VALUES`, `DEFAULT` trong `VALUES` và trong `UPDATE ... SET`, `OVERRIDING SYSTEM VALUE | USER VALUE`. Ghi giá trị vào cột `IDENTITY ALWAYS` trả `428C9`, như PG.
  - **Khác PG:** `CREATE/ALTER/DROP SEQUENCE` có hiệu lực ngay, không thuộc giao dịch (như `CREATE TABLE`). Giá trị mặc định chỉ áp dụng cho câu SQL. API HTTP `/v1/write` ghi đúng dòng được gửi.
- `CHECK (expr)` ở cột hoặc bảng, có thể đặt tên bằng `CONSTRAINT tên`. Tên tự sinh như PG (`bảng_cột_check`, `bảng_check`, `bảng_check1`). Vi phạm trả `23514`, NULL thì qua, như PG. Không được dùng subquery, aggregate hay hàm phiên. Kiểm khi INSERT, UPDATE và `ON CONFLICT DO UPDATE` bằng SQL. API HTTP không kiểm.
- `DROP TABLE [IF EXISTS] a [, b ...] [CASCADE | RESTRICT]` và `TRUNCATE [TABLE] [ONLY] a [, b ...] [RESTART IDENTITY | CONTINUE IDENTITY] [CASCADE | RESTRICT]`:
  - Như PG, mọi bảng phải tồn tại (hoặc có `IF EXISTS`) trước khi bảng nào bị đổi. Chưa có bảng nào phụ thuộc bảng khác (chưa có view, khoá ngoại, sequence), nên `CASCADE`/`RESTRICT` và `RESTART IDENTITY` không khác nhau.
  - `TRUNCATE` chạy O(1) như PG: bảng được thay bằng một bảng rỗng mang ID mới, cùng tên, schema và OID. Dòng cũ được thu hồi như dữ liệu của bảng đã drop. Giống PG ([TRUNCATE không an toàn với MVCC](https://www.postgresql.org/docs/16/sql-truncate.html)), giao dịch có snapshot từ trước sẽ thấy bảng rỗng.
  - Câu ghi autocommit đang chạy đúng lúc `TRUNCATE` thì được tự chạy lại trên bảng mới. Trong giao dịch tường minh, câu ghi đó trả `40001`.
  - **Khác PG:** cả hai có hiệu lực ngay, kể cả khi chạy trong `BEGIN` (như `CREATE TABLE`), và nhiều bảng thì được đổi lần lượt, không nguyên tử.
  - `DROP TABLE` cũng xoá bảng khỏi publication và khỏi `REPLICA IDENTITY FULL`, như PG.
- `INSERT ... VALUES (...), (...)` hoặc `INSERT ... SELECT`, kèm `ON CONFLICT [(cột)] DO NOTHING | DO UPDATE SET ... [WHERE ...]` (có `excluded.`) và `RETURNING`.
- `UPDATE ... SET ... [FROM ...] WHERE ... RETURNING` (được phép đổi khoá chính), `DELETE ... [USING ...] WHERE ... RETURNING`. WHERE và SET được chứa subquery.
- `SELECT` đầy đủ (xem §9):
  - JOIN các loại: `INNER/LEFT/RIGHT/FULL/CROSS/NATURAL`, `USING`, `LATERAL`, danh sách bảng ngăn bởi dấu phẩy;
  - subquery: vô hướng, `EXISTS`, `IN`, `ANY/ALL`, có tương quan, trong FROM;
  - `WITH` (kể cả `RECURSIVE`), `UNION/INTERSECT/EXCEPT [ALL]`;
  - window function với frame `ROWS/RANGE/GROUPS`;
  - `GROUPING SETS/ROLLUP/CUBE` và `grouping()`, `DISTINCT ON`, `NULLS FIRST/LAST`, `FETCH FIRST`;
  - `VALUES`, `TABLE t`, `generate_series`, kể cả trong danh sách SELECT.
- Biểu thức: số học, `||`, so sánh, `AND/OR/NOT` theo logic ba giá trị, `IS [NOT] NULL`, `IS [NOT] DISTINCT FROM`, `IN`, `BETWEEN [SYMMETRIC]`, `LIKE/ILIKE`, `~ ~* !~ !~*`, `CASE`, `CAST(...)`, `::`, `INTERVAL '...'`, `ARRAY[...]`, row constructor, tham số `$n`.
- Kiểu phát sinh từ biểu thức: `date` (OID 1082), `interval` (1186) và `text[]` (1009), kèm phép tính thời gian kiểu PG (`timestamp ± interval`, `date - date`, ...).
- Hàm vô hướng và aggregate của PostgreSQL và ClickHouse: xem danh sách ở §9.
- Giao dịch: `BEGIN [ISOLATION LEVEL ...]`, `COMMIT`, `ROLLBACK`, `SET TRANSACTION ISOLATION LEVEL`. Có thêm `SET` và `SHOW` cho các biến phiên mà driver hay hỏi.

**Mức cô lập:**

| Yêu cầu | Thực tế |
|---|---|
| `READ COMMITTED` (mặc định của PG), `REPEATABLE READ` | Snapshot Isolation |
| `SERIALIZABLE` | Serializable thật (OCC kiểm tra tập đọc) |

Câu lệnh đơn chạy ngoài `BEGIN` (autocommit) mà va chạm ghi sẽ được **server tự chạy lại** trên snapshot mới. Client không thấy lỗi, và không mất cập nhật: test cho 8 phiên cùng `UPDATE ... SET bal = bal + 1` trên một dòng, kết quả đúng tuyệt đối.

Trong giao dịch tường minh, va chạm trả `40001` (`could not serialize access`), và ứng dụng phải chạy lại cả giao dịch. Đây cũng là điều PostgreSQL yêu cầu ở mức `REPEATABLE READ` trở lên.

**Đường truy cập.** `WHERE khoá = c` và `WHERE khoá IN (...)` đi thẳng vào chỉ mục chính. Mọi điều kiện khác quét toàn bảng theo snapshot. Giao dịch luôn thấy dữ liệu chính nó vừa ghi.

**Lỗi** mang SQLSTATE chuẩn, nên code xử lý lỗi viết cho PostgreSQL vẫn dùng được:

| SQLSTATE | Ý nghĩa |
|---|---|
| 23505 | trùng khoá |
| 23502 | NOT NULL |
| 40001 | xung đột giao dịch |
| 25P02 | giao dịch đã lỗi |
| 22012 | chia cho 0 |
| 42P01 / 42703 | bảng / cột không tồn tại |
| 42601 | lỗi cú pháp, có vị trí |
| 72000 | snapshot quá cũ, lịch sử đã bị GC |

**Giao thức:**
- Simple query, có thể nhiều câu lệnh trong một lần gửi.
- Extended query: `Parse/Bind/Describe/Execute/Sync/Close/Flush`, có prepared statement đặt tên, `PortalSuspended` khi giới hạn số dòng, và pipeline/batch.
- Tham số và kết quả ở cả dạng text lẫn binary.
- Chế độ lỗi bỏ qua message đến khi gặp `Sync`, đúng chuẩn.
- TLS (`SSLRequest` và direct SSL của PostgreSQL 17) và SCRAM-SHA-256, xem §10. Không có chứng chỉ thì server trả lời `SSLRequest` bằng `N`.

## 3. Chưa hỗ trợ

- `CREATE VIEW`, `ARRAY JOIN` của ClickHouse, `FINAL`/`SAMPLE` (được chấp nhận nhưng bỏ qua), `numeric` chính xác.
- Truy vấn nhiều bảng chạy bằng bộ thực thi tổng quát trên dữ liệu đã đưa hết vào bộ nhớ (hash join, không spill ra đĩa). Chỉ aggregate một bảng mới dùng view dạng cột.
- Index phụ. Mọi truy vấn không theo khoá chính đều quét bảng.
- `ALTER COLUMN ... TYPE`, khoá ngoại, `UNIQUE` ngoài khoá chính. `ALTER TABLE` đã có, xem §11. `COPY` text/csv đã có, xem §14 (chưa có binary, file/program, `FORCE_*`, giao thức mở rộng).
- `pg_catalog` và `information_schema`. Lệnh `\d` của psql, cùng phần duyệt schema của DBeaver và DataGrip, sẽ báo lỗi. Truy vấn dữ liệu thì vẫn chạy.
- Xác thực bằng chứng chỉ client, GSSAPI, nhiều user/role (xem §10). (Huỷ câu lệnh và các timeout của session đã có từ 2026-10-29, xem §11.)
- Chưa truy vấn được time-series, tìm kiếm, `as_of` từ SQL. Các chức năng này vẫn có qua HTTP API.

Vì các giới hạn này, **ORM như Hibernate/Jmix chưa thể dùng STASK làm datastore chính**. Chúng cần `information_schema`, sequence, JOIN và DDL đầy đủ. Hướng dùng hiện tại vẫn là: ứng dụng giữ PostgreSQL cho phần nghiệp vụ, còn service layer gọi STASK qua JDBC bằng SQL viết tay. Đây là một cách mới bên cạnh HTTP API.

## 4. Kiểm chứng

- `internal/sql`: kiểu dữ liệu, biểu thức, gom nhóm, lỗi kèm SQLSTATE, upsert/RETURNING, đổi khoá chính, giao dịch (đọc dữ liệu tự ghi, rollback, first-committer-wins, trạng thái lỗi 25P02), autocommit tranh chấp không mất cập nhật, suy luận kiểu cho prepared statement. Chạy kèm race detector.
- `internal/pgwire` với driver **pgx v5** thật: tham số binary đủ 5 kiểu, NULL, lỗi giữ kết nối dùng tiếp, giao dịch qua API driver, batch pipeline. Có thêm pool 32 kết nối thực hiện 1.600 lượt chuyển tiền đồng thời: tổng tiền được bảo toàn, và sai mật khẩu thì bị từ chối.
- **psql 16 thật** (image docker `postgres:16`): DDL, DML, giao dịch, lỗi, tiếng Việt UTF-8.

## 5. Stress test theo số kết nối

Xem mục kết quả bên dưới. Kịch bản (`scripts/stress.sh`, `cmd/staskstress`):
- Mở N kết nối đồng thời, rải trên 40 địa chỉ nguồn `127.0.0.x` để vượt giới hạn ~64k cổng tạm cho mỗi cặp địa chỉ.
- **Burst:** mọi kết nối gửi **đúng một request vào cùng một thời điểm** (N request đồng thời).
- **Tải duy trì:** vòng kín trong 30–60s, 80% `SELECT` theo khoá + 20% `UPDATE v = v + 1` (mỗi `UPDATE` là một giao dịch có fsync), trên 100k dòng.

Client và server chạy chung VM 8 vCPU / 31GB, nên số liệu là cận dưới của một server tách riêng.

### Kết quả (VM 8 vCPU / 31GB, 2026-10-27)

| Số kết nối | Mở kết nối | Burst: N request cùng lúc | Tải duy trì | RAM server |
|---|---|---|---|---|
| 1.000 | 66ms, lỗi 0 | xong hết trong **17ms** (p99 15.7ms) | **112k req/s**, p50 2.8ms, p99 51ms | 121MB |
| 10.000 | 383ms, lỗi 0 | xong hết trong **168ms** (p99 149ms) | **107k req/s**, p50 3.2ms, p99 0.8s | 226MB nhàn rỗi, 525MB khi tải |
| 100.000 | 3.4s, lỗi 0 | xong hết trong **1.87s** (p50 0.77s, p99 1.42s) | **91k req/s**, p50 2.3ms, p99 12.4s | 2.0GB nhàn rỗi, 4.6GB khi tải |
| 1.000.000, không giới hạn | **39s, mở đủ 1M, lỗi 0** | **server bị OOM-kill** sau 543k phản hồi | — | 19.4GB nhàn rỗi (~20KB/kết nối) |
| 1.000.000, `-pg-max-conns 100000` | nhận 100k, từ chối 900k bằng `53300` | 100k xong trong 1.53s | 88k req/s | 4.1–4.6GB, server sống |

Không có request nào lỗi ở các mức 1k, 10k và 100k.

Cách đọc kết quả:
- **Thông lượng gần như không đổi theo số kết nối, khoảng 90–110k req/s.** Đây là trần CPU: client và server dùng chung 8 vCPU, CPU luôn ở mức 95%. Đĩa không phải nút thắt, vì group commit gom các `UPDATE` vào chung lần fsync.
- **Khi kết nối nhiều hơn khả năng phục vụ, độ trễ tăng tuyến tính theo định luật Little:** độ trễ trung bình ≈ N / thông lượng. Với 100k kết nối luôn bận, trung bình ~1.1s. p50 vẫn 2.3ms còn p99 lên 12s: hàng đợi không công bằng, vì chưa có lập lịch công bằng giữa các phiên.
- **Burst đo đúng câu hỏi "N request cùng lúc thì bao lâu xong hết":** 1k mất 17ms, 10k mất 0.17s, 100k mất 1.9s. Tỷ lệ gần tuyến tính, đúng với một hệ đã bão hoà CPU.
- **Giới hạn ở mức 1M là RAM, không phải socket hay cổng.** Kernel và runtime giữ được đủ 1M kết nối mở (40 IP nguồn, nofile 2.1M). Nhưng 20KB/kết nối phía server cộng với bộ đệm socket của kernel cho cả hai đầu đã vượt 31GB khi cả 1M kết nối cùng hoạt động.
- Vì vậy giờ đã có `-pg-max-conns` (mặc định 100.000), giống `max_connections` của PostgreSQL. Phần vượt bị từ chối sạch thay vì kéo cả server chết.

So sánh: PostgreSQL dùng một tiến trình cho mỗi kết nối (vài MB/kết nối). Thực tế nó cần PgBouncer khi vượt vài nghìn kết nối. STASK giữ được 100k kết nối với khoảng 2GB.

Hướng cải thiện, để sau khi engine hoàn chỉnh:
- Bộ đệm đọc/ghi cấp phát lười. Hiện mỗi kết nối giữ 16KB bộ đệm kể cả khi nhàn rỗi; có thể giảm xuống dưới 5KB.
- Lập lịch công bằng giữa các phiên, để cắt đuôi p99 khi quá tải.
- Tách client và server ra hai máy để đo đúng trần của server.

Tái lập kết quả: `scripts/stress.sh 1000 10000 100000 1000000`. Script tạm thời nâng các tham số sysctl và nofile cho lần chạy, không lưu vĩnh viễn. Kết quả nằm ở `results/stress/`.

### Đo lại với code hiện tại (2026-10-29, `results/v8/stress`)

| Số kết nối | Burst: N request cùng lúc | Tải duy trì | RAM server nhàn rỗi / khi tải |
|---|---|---|---|
| 1.000 | xong trong 14 ms (17 trước) | **137k req/s** (112k), p50 5,8 ms, p99 28 ms (51) | 115 MB / 250 MB |
| 10.000 | xong trong 177 ms (168) | **119k req/s** (107k), p50 81 ms (3,2), **p99 163 ms (0,8 s)** | 214 MB / 1,3 GB |
| 100.000 | xong trong 1,88 s (1,87) | **103k req/s** (91k), p50 0,84 s (2,3 ms), **p99 2,0 s (12,4 s)** | 1,6 GB / 5,6 GB |

Không có request nào lỗi. Thông lượng tăng 10–22%. Khi quá tải, phân bố độ trễ giờ **công bằng hơn**: p50 gần với độ trễ trung bình theo định luật Little (100k kết nối / 103k req/s ≈ 1 s), còn đuôi p99 giảm 5–6 lần. Trước đây một số phiên được phục vụ liên tục (p50 rất thấp) trong khi các phiên khác chờ tới 12 s. RAM khi tải ở mức 100k tăng từ 4,6 lên 5,6 GB. Mức 1 triệu kết nối không đo lại (giới hạn `-pg-max-conns` không đổi).

## 6. Độ trễ commit so với PostgreSQL 16

Kịch bản: INSERT từng dòng, mỗi câu lệnh là một giao dịch autocommit có fsync. 32 kết nối ghi, 200k dòng, qua driver pgx. Cùng VM và cùng ổ đĩa; PostgreSQL chạy với `synchronous_commit=on`, `fsync=on`. Mỗi kịch bản chạy 3 lần, bảng ghi trung vị (dao động giữa các lần chỉ ±0.05ms). Tái lập bằng `scripts/latcompare.sh`.

| Kịch bản | STASK p50 / p99 | PG p50 / p99 | STASK dòng/s | PG dòng/s | p99 SELECT STASK / PG |
|---|---|---|---|---|---|
| Chỉ ghi | **1.41 / 2.30ms** | 1.67 / 2.46ms | **21.8k** | 18.5k | — |
| Ghi + 30k SELECT/s | **1.62 / 3.11ms** | 2.24 / 4.13ms | **19.0k** | 13.5k | **1.6ms** / 28.7ms |
| Ghi + 42k SELECT/s | **1.78 / 3.63ms** | 2.45 / 4.87ms | **17.1k** | 12.3k | **3.5ms** / 589ms |
| Ghi + 16 luồng đọc vòng kín | **2.23** / 5.75ms | 2.44 / **4.74ms** | **13.0k** | 12.4k | 0.9 / 1.25ms |

Ở kịch bản cuối, cả hai hệ đều chạy 100% CPU. STASK phục vụ **68k SELECT/s**, PostgreSQL 42k/s. Vì đọc nhanh hơn nên luồng đọc của STASK chiếm nhiều CPU hơn, và p99 của phần ghi thua PostgreSQL. Với cùng một lượng đọc (hai dòng 30k và 42k), STASK hơn ở mọi chỉ số.

Độ trễ giảm được là nhờ ba thay đổi (docs/03 §3), đo trên kịch bản "chỉ ghi":

| Bước | p50 | dòng/s |
|---|---|---|
| Ban đầu | 2.68ms | 11.8k |
| Ghi số 0 trước cả segment đầu tiên: fsync 1.25 → 0.8ms | 1.77ms | 17.5k |
| Đường ống committer/syncer: CPU ra khỏi đường găng | 1.70ms | 18.2k |
| Gom thích ứng: 32 client vào chung một lô thay vì hai | **1.41ms** | **21.8k** |

Giới hạn dưới là phần cứng: một lần flush trên ổ ảo này mất ~0.74ms, và flush song song không nhanh hơn. Muốn xuống dưới ~1ms thì cần SSD có tụ chống mất điện (PLP), hoặc bền vững nhờ nhân bản (xác nhận ghi khi quorum đã nhận).

Các thay đổi đã được kiểm chứng lại độ bền bằng:
- toàn bộ test kèm race detector, gồm 40 lượt mô phỏng mất điện có GC chạy cùng và test lỗi fsync khi chuyển segment;
- `staskchaos` 30 chu kỳ kill / ENOSPC / EIO;
- 12 lần cắt điện thật bằng dm-flakey.

Kết quả ở `results/chaos-pipe.txt` và `results/powercut-pipe.txt`.

### Đo lại với code hiện tại (2026-10-29, `results/v8/latcompare.txt`)

Cùng kịch bản và cùng VM, PG chạy lại cùng lượt; trung vị của 3 lần.

| Kịch bản | STASK p50 / p99 | PG p50 / p99 | STASK dòng/s | PG dòng/s | p99 SELECT STASK / PG |
|---|---|---|---|---|---|
| Chỉ ghi | **1,28 / 2,36 ms** | 1,68 / **2,18 ms** | **23,9k** | 18,6k | — |
| Ghi + 30k SELECT/s | **1,35 / 2,38 ms** | 1,98 / 3,53 ms | **23,0k** | 15,3k | **1,0 ms** / 18,2 ms |
| Ghi + 42k SELECT/s | **1,42 / 2,54 ms** | 2,21 / 4,18 ms | **21,8k** | 13,7k | **1,3 ms** / 63,7 ms |
| Ghi + 16 luồng đọc vòng kín | **1,65 / 3,40 ms** | 2,29 / 4,27 ms | **18,2k** | 13,3k | **0,7** / 1,1 ms |

So với lần đo trước, STASK nhanh hơn ở mọi dòng (p50 chỉ ghi 1,41 → 1,28 ms; ghi kèm đọc vòng kín 13,0k → 18,2k dòng/s, p99 5,75 → 3,40 ms). Ở kịch bản vòng kín, STASK phục vụ **77k SELECT/s** so với 49k của PG. Kịch bản này trước đây STASK thua PG ở p99 ghi, giờ đã thắng. Chỉ còn p99 của kịch bản "chỉ ghi" là PG nhỉnh hơn (2,18 so với 2,36 ms).

## 7. Nạp hàng loạt so với PostgreSQL 16

Kịch bản: 1M dòng, mỗi câu lệnh 100 dòng, 32 kết nối ghi, 16 kết nối đọc điểm song song. Chạy 2 lần trên máy sạch; tái lập bằng `scripts/bulkcompare.sh`.

| | STASK | PostgreSQL 16 |
|---|---|---|
| INSERT 1M (lô 100) | **3.16–3.25s** (~310k dòng/s) | 5.52–5.75s |
| UPDATE 1M (lô 100) | **2.58–2.62s** | 7.15–7.18s |
| UPDATE toàn bảng, 1 giao dịch | **8.05–8.59s** | 10.66–11.27s |
| INSERT 10M (lô 1000, không có đọc) | **23.4s** | 27.25s |
| p99 SELECT trong lúc ghi | 5.6–6.3ms | **3.1–3.4ms** |

Có được kết quả này nhờ trả lời client ngay sau fsync (lớp phủ đọc) và chỉ mục bất biến, đọc không cần khoá (docs/03 §3). Sau thay đổi, độ bền đã được kiểm chứng lại: chaos 30 chu kỳ và 12 lần cắt điện thật, kết quả RPO=0, 0 giao dịch đã xác nhận bị mất (`results/chaos-overlay.txt`, `results/powercut-overlay.txt`).

Không có lần đọc nào trả về không tìm thấy (`not_found` = 0) trên cả hai hệ. Sau khi khởi động lại, `count` và `sum(amount)` khớp đúng với trước đó.

### Đo lại với code hiện tại (2026-10-29, `results/v8/bulkcompare.txt`)

| | STASK | PostgreSQL 16 |
|---|---|---|
| INSERT 1M (lô 100) | **2,66–2,69 s** (~375k dòng/s; trước 3,16–3,25 s) | 5,15–5,19 s |
| UPDATE 1M (lô 100) | **1,94–1,96 s** (trước 2,58–2,62 s) | 6,95–7,10 s |
| UPDATE toàn bảng, 1 giao dịch | **6,28–6,31 s** (trước 8,05–8,59 s) | 11,28–11,66 s |
| INSERT 10M (lô 1000, không có đọc) | **17,8 s** (trước 23,4 s) | 25,8 s |
| p99 SELECT trong lúc ghi | 4,1–5,0 ms (trước 5,6–6,3) | **3,0–3,1 ms** |

STASK nhanh hơn 20–25% so với lần trước và gấp 1,4–3,6 lần PG. Điểm duy nhất PG còn hơn là p99 của đọc điểm chạy song song với nạp hàng loạt. `not_found` = 0 ở cả hai hệ.

## 8. Aggregate SQL chạy trên view dạng cột

Với bảng `WITH (columnar = true)`, một câu `SELECT` aggregate autocommit được tính nhóm trên view dạng cột thay vì quét dòng (`internal/sql/views.go`). Phần sau khi gom nhóm (HAVING, danh sách cột, ORDER BY, LIMIT, kiểu kết quả) vẫn chạy ở tầng SQL như cũ, nên kết quả trùng với đường quét dòng.

**Điều kiện dùng view:**
- Một bảng.
- GROUP BY trên cột (không phải cột `DOUBLE PRECISION`).
- Aggregate là `count`/`sum`/`avg`/`min`/`max` không DISTINCT, đối số là cột. `sum`/`avg`/`min`/`max` chỉ trên cột số.
- WHERE là các điều kiện nối bằng AND và OR, mỗi điều kiện có một trong các dạng:
  - `cột op hằng`, đặt hằng ở bên nào cũng được;
  - `cột [NOT] IN (hằng, ...)`;
  - `cột BETWEEN hằng AND hằng`;
  - `cột IS [NOT] NULL`.
- GROUP BY là cột, hoặc `date_trunc('đơn vị', cột thời gian)`. Đơn vị gồm `milliseconds` … `day`, `week`, `month`, `quarter`, `year`, `decade`, tính theo UTC như mọi phiên STASK. Khi đó cột thời gian chỉ được xuất hiện trong SELECT, HAVING, ORDER BY dưới dạng đúng biểu thức `date_trunc` đó, hoặc bên trong aggregate (như kiểm tra GROUP BY của PG).
- Aggregate gồm `count`, `sum`, `avg`, `min`, `max`, `min`/`max` của cột thời gian, và `count(DISTINCT cột)` khi mọi DISTINCT cùng một cột không phải số thực.
- Hằng được ép kiểu giống hệt `compare()` của đường quét dòng: chuỗi đổi sang kiểu của cột (ví dụ `created >= '2026-10-15'`), số nguyên đổi sang số thực khi cột là số thực. Hằng mà đường quét dòng so sánh theo cách khác thì không được đẩy xuống view.
- Không phải tra cứu theo khoá chính.
- **Không nằm trong BEGIN ... COMMIT.** View phản ánh ít nhất snapshot của câu lệnh nhưng có thể chứa commit muộn hơn, điều mà giao dịch repeatable read không được thấy.
- Câu không đủ điều kiện sẽ chạy đường quét dòng như trước.
- **View bị trễ** (ví dụ sau một lần nạp lớn, hoặc replica đang đuổi theo): câu lệnh chờ view tới snapshot của nó, chừng nào view còn đang tiến, giống learner read của [TiFlash](https://docs.pingcap.com/tidb/dev/tiflash-overview/). Chỉ khi view đứng yên 500 ms, hoặc chờ quá 10 s, câu lệnh mới quét dòng.
- **`SET stask.read_staleness = '2s'`** (hoặc số mili giây): phiên chấp nhận đọc view trễ tới mức đó mà không cần chờ, giống [`tidb_read_staleness`](https://docs.pingcap.com/tidb/dev/tidb-read-staleness/). Mặc định là 0.

ORDER BY một aggregate hoặc một cột nhóm kèm LIMIT (không HAVING, không DISTINCT) chỉ lấy `OFFSET+LIMIT` nhóm đầu bằng một heap. NULL được xếp như PostgreSQL: sau cùng khi tăng, đầu tiên khi giảm. `sum` trên tập rỗng trả NULL.

**Gom nhóm trong view (`internal/proj/columnar.go`):**
- **Gom theo lô (vector hoá):** mỗi aggregate chạy một vòng lặp chặt trên cả lô dòng, thay vì diễn giải từng aggregate cho từng dòng.
- **Nhiều khoá:** pass đầu chia dòng (kèm khoá đã tính) vào tối đa 1.024 phân vùng theo dải khoá, mỗi phân vùng có khoảng 512 KB bộ tích luỹ nên nằm vừa cache. Đây là radix partitioning theo [Müller et al., SIGMOD 2015](https://rmarcus.info/dbscholar/papers/hc6fc8717e8f0c055). Bộ đệm được tái sử dụng giữa các truy vấn để GC không phải quét lại heap của bảng.
- **`count(DISTINCT x)` hai pha**, như [`RewriteDistinctAggregates` của Spark](https://developer.cloud.tencent.com/article/2379577): pha 1 GROUP BY (nhóm…, x), dùng nguyên đường dense/radix, với các aggregate khác tính thành từng phần (count, sum, min, max); pha 2 gộp theo nhóm, đếm các x khác NULL. Oracle test có bản cài riêng bằng set để đối chiếu.
- **`date_trunc` thành số thứ tự bucket** (ngày, tháng… kể từ epoch, tính lịch bằng thuật toán của H. Hinnant): khoá nhóm có dải nhỏ, nên chạy đường dense. Cột bucket được tính song song mỗi truy vấn, bộ đệm tái sử dụng.
- **Hàng đợi việc cho các luồng phụ** (work stealing): mỗi luồng lấy phần việc tiếp theo khi xong phần trước. Trước đây, phần không xin được luồng phụ chạy tuần tự trên luồng chính; qua SQL (chỉ 2 luồng phụ rảnh) q3 vì thế tốn 95 ms thay vì 59 ms.
- **Top-k không cấp phát, chạy song song:** so sánh bằng số trực tiếp; giá trị khoá chỉ được tạo cho các nhóm được giữ lại. Mỗi phần của bitmap nhóm chọn top-k riêng, sau đó mới gộp.
- **View báo tiến độ** sau mỗi 16.384 thao tác hoặc 20 ms. Trước đây là sau mỗi 4.096 bản ghi; với đợt nạp lớn, mỗi bản ghi chứa hàng trăm dòng nên view vài giây không báo tiến độ. Câu lệnh đang chờ kết luận nhầm là view kẹt và quét dòng, rồi chính lần quét đó lại lấy CPU của view (thấy trong profile replica: 17% CPU).
- **Dense:** khi khoá nhóm là mã từ điển, hoặc số nguyên có khoảng giá trị không lớn hơn khoảng 2 lần số dòng, bộ tích luỹ nằm trong một mảng phẳng đánh chỉ số theo khoá. Không băm, không cấp phát theo nhóm, giống `FixedHashMap` của ClickHouse.
  - Ít khoá: mỗi worker có mảng riêng rồi cộng lại.
  - Nhiều khoá: worker chỉ chia vị trí dòng vào các phân vùng theo dải khoá (căn theo 64), rồi mỗi phân vùng được một goroutine tích luỹ. Không có khoá (lock), không ghi chung.
- **Hash:** các trường hợp khác dùng bảng băm theo worker, chia phân vùng theo hash, gộp song song theo phân vùng.
- **Cột TEXT** được mã hoá từ điển như `VARCHAR` cho tới khi vượt 65.536 giá trị khác nhau. Khi đó cột được coi là văn bản tự do và bị bỏ khỏi view (như `LowCardinality` của ClickHouse quay về chuỗi thường). Truy vấn cần cột đó sẽ quét dòng.
- Song song qua `spawnHelper`/`keepHelper` của tầng SQL: aggregate trên view vẫn nhường core cho đọc/ghi foreground như aggregate quét dòng.

**Kiểm chứng:**
- `TestViewAggregatesMatchRowPath`: 14 câu (NULL, HAVING, OFFSET, tham số, TEXT, cột nhóm có bí danh bảng), mỗi câu chạy trên session có view và session không có view, kết quả phải trùng. Test cũng kiểm tra đúng câu nào được trả lời từ view, và câu trong BEGIN thì không.
- `TestColumnarAggregateMatchesRows`: ba đường gom nhóm (ít khoá, nhiều khoá, khoá thưa) so với bộ gom theo dòng, có NULL, xoá, cập nhật, và helper bị từ chối hoặc dừng giữa chừng. Chạy kèm race detector.

**Kết quả** (2 triệu dòng, server 3 core, client 3 core khác, p50, 3 lượt, mọi hệ cùng checksum kết quả; số ClickHouse 25.8 và PG16 lấy từ `results/ch`):

| Truy vấn | ClickHouse | PG16 | STASK SQL trước | **STASK SQL sau** |
|---|---|---|---|---|
| q1 `GROUP BY status` (TEXT) | 16,3 ms | 470 ms | 581 ms | **7,1–8,0 ms** |
| q2 lọc `amount > 9000` + agg | 10,5 ms | 189 ms | 324 ms | **8,0 ms** |
| q3 top 10 khách (khoảng 860k nhóm) | 88 ms | 2.373 ms | 14.681 ms | **59–61 ms** |
| q4 đọc theo id | 3,57 ms | 0,18 ms | 0,10 ms | 0,11–0,13 ms |
| q5 `GROUP BY date_trunc('day', ...)` | 32,6 ms | 2.372 ms | – | **29,6 ms** |
| q6 `count(DISTINCT customer_id) GROUP BY status` | 131,6 ms | 9.110 ms | 10.390 ms | **150 ms** (HTTP: 86,5 ms) |

Cột "sau" là `results/ch` (3 lượt) cho q1–q4 và `results/ch` lần chạy lại ngày 2026-10-28 cho q5–q6, sau hai lần sửa:
- **q5 sai đáp án ở lần đo đầu**, dù view tính đúng. Nguyên nhân: `typeOf` báo cột `date_trunc(...)` là `text`, nên client nhận chuỗi `2025-01-01 00:00:00+00` thay vì timestamptz. Sau khi sửa, checksum trùng PG và ClickHouse.
- **q6 mất 10 giây.** Hai pha kiểu Spark tạo khoảng 1 triệu nhóm (status, customer_id) ở pha 1 rồi đóng gói thành dòng kết quả. Cách mới làm như hash set chia hai tầng của [ClickHouse](https://kb.altinity.com/altinity-kb-queries-and-syntax/group-by) (256 bucket theo hash, mỗi bucket gộp độc lập) (`internal/proj/distinct_fast.go`):
  - mỗi worker đẩy cặp (khoá nhóm, x) vào P phân vùng theo hash;
  - mỗi phân vùng được một goroutine khử trùng bằng hash set địa chỉ mở rồi đếm theo nhóm.
  
  Kết quả nhanh hơn khoảng 70 lần. Đường hai pha cũ chỉ còn dùng cho GROUP BY nhiều cột.

`results/sqlcol.out`: server ghi nhận 160 câu được trả lời từ view, khớp với số câu aggregate đã gửi.

## 9. Cú pháp đầy đủ PostgreSQL và ClickHouse (2026-10-28)

**Cách làm.** Đường nhanh một bảng (§8, cùng các view) được giữ nguyên. Câu nào dùng tính năng mà đường nhanh không có thì sang bộ thực thi quan hệ tổng quát (`internal/sql/query.go`). `route.go` quyết định theo cú pháp. Nếu đường nhanh báo "cột/hàm không tồn tại", câu đó được chạy lại trên đường tổng quát (ví dụ alias của ClickHouse dùng trong WHERE).

**Bộ thực thi tổng quát.** Câu lệnh được biên dịch một lần thành plan:
- **Binder:** tên cột thành `Slot` (vị trí trong dòng làm việc), hoặc `OuterRef{độ sâu}` cho subquery tương quan. Độ sâu được đếm theo cấp truy vấn chứ không theo scope, nên CTE xen giữa không làm lệch.
- **Thứ tự chạy:**
  1. FROM, gồm hash join với khoá lấy từ ON/USING. Điều kiện bằng trong WHERE được đẩy xuống join trong cùng nhất có đủ cột, nên `FROM a, b WHERE a.x = b.y` cũng là hash join.
  2. WHERE.
  3. GROUP BY / GROUPING SETS: mỗi dòng vào mọi tập nhóm; khoá không thuộc tập thì NULL. Với `WITH ROLLUP` của ClickHouse thì khoá đó lấy giá trị mặc định của kiểu.
  4. HAVING.
  5. Window.
  6. Danh sách SELECT, kèm hàm trả về tập.
  7. DISTINCT, ORDER BY, DISTINCT ON, LIMIT BY, OFFSET/LIMIT.
- Set operation và CTE đệ quy bọc ngoài pipeline này.
- **Subquery không tương quan:** tính một lần. `IN (subquery)` thành hash set, xử lý NULL theo logic ba giá trị.
- **Window:** frame có `UNBOUNDED PRECEDING` cộng dồn tăng dần; frame khác tính lại theo từng dòng.

**Hàm đã có:**
- **Aggregate PG:**
  - `count sum avg min max`, `string_agg`, `array_agg` (có `DISTINCT`, `ORDER BY` bên trong);
  - `bool_and bool_or every`, `stddev[_samp|_pop] variance var_samp var_pop`;
  - `percentile_cont percentile_disc mode` (`WITHIN GROUP`), và `FILTER (WHERE ...)` cho mọi aggregate.
- **Aggregate ClickHouse:**
  - `countIf sumIf avgIf minIf maxIf`, `uniq uniqExact`;
  - `argMax argMin`, `any anyLast`, `groupArray groupUniqArray`;
  - `quantileExact[Low|High](p)(x)`, `median`;
  - `stddevSamp varPop`, …
- **Window:** `row_number rank dense_rank percent_rank cume_dist ntile lag lead first_value last_value nth_value`, cộng mọi aggregate ở trên.
- **Chuỗi:**
  - `replace left right lpad rpad reverse repeat split_part initcap md5 format to_char`;
  - `starts_with strpos/position regexp_like regexp_replace ascii chr concat_ws btrim(s, ký tự)`;
  - phía ClickHouse: `replaceAll startsWith endsWith match lowerUTF8 ...`.
- **Số:** `sign mod div power sqrt cbrt exp ln log log10 pi greatest least`, cộng `intDiv modulo` của ClickHouse.
- **Thời gian:**
  - PG: `date_part/extract` (đủ trường, kể cả `dow isodow doy quarter epoch`), `make_date make_timestamp age`, `to_char(timestamp, mẫu)`;
  - ClickHouse: `toStartOf*`, `toYear/Month/DayOfMonth/Hour/DayOfWeek/YYYYMM`, `toDate toDateTime formatDateTime dateDiff addDays/Hours/...`.
- **Ép kiểu và điều kiện của ClickHouse:** `toInt64 toFloat64 toString toUInt8 toInt64OrZero if multiIf ifNull isNull isNotNull ?:`.
- **Cú pháp riêng của ClickHouse:**
  - `count()`, `LIMIT n BY`, `LIMIT a, b`, `PREWHERE`, `SETTINGS`, `FORMAT`;
  - `WITH biểu thức AS tên`, `GLOBAL IN`, `ANY/ALL JOIN`, `WITH ROLLUP/CUBE/TOTALS`;
  - kiểu `Int64/String/Nullable/LowCardinality/DateTime64`, mảng `[...]`, `numbers(n)`, alias dùng được ở mọi mệnh đề và lồng nhau.

**`SET sql_dialect = 'clickhouse'`** (mặc định `postgres`, tương tự biến cùng tên của [Apache Doris](https://doris.apache.org/docs/3.0/lakehouse/sql-dialect)). Biến này chỉ đổi những quy tắc mà hai hệ mâu thuẫn nhau:
- Với `SELECT CAST(id AS String) FROM t ORDER BY id`, PostgreSQL sắp theo cột đầu ra (cũng mang tên `id`).
- ClickHouse đặt tên cột đó là `CAST(id, 'String')`, nên `id` vẫn là cột đầu vào.

Cú pháp riêng của từng hệ thì dùng được ở cả hai chế độ.

**Kiểm chứng: bộ đối chiếu `cmd/sqlcorpus` (`scripts/sqlcorpus.sh`).**
- 188 câu chạy trên cùng dữ liệu: customers 200, orders 5.000 (có NULL), items 12.000.
- Mỗi câu được so từng dòng với PostgreSQL 16 (147 câu PG) hoặc ClickHouse 25.8 (41 câu ClickHouse). Câu không có ORDER BY thì sắp xếp dòng trước khi so.
- Bộ test có cả DML: INSERT…SELECT, UPDATE…FROM, DELETE…USING rồi đọc lại.

| Lần chạy | PG | ClickHouse | Ghi chú |
|---|---|---|---|
| `run1` (131 câu đầu) | 43/98 (44%) | 20/33 (61%) | lỗi `rebuild` làm mất mọi biểu thức đi qua bước thay alias |
| `run2` | 91/98 | 30/33 | |
| `run4` | 98/98 | 33/33 | thêm `sql_dialect` |
| `run5` (+57 câu khó hơn) | 144/147 | 38/40 | |
| **`final`** | **147/147 (100%)** | **41/41 (100%)** | `results/sqlcorpus/final.md` |

Các lỗi tìm được trên đường và cách sửa:
- `ORDER BY` theo tên cột đầu ra phải thắng cột đầu vào (PG). Đường nhanh cũ chỉ xét alias tường minh.
- Ép kiểu số thực sang số nguyên phải làm tròn (`rint`), không được báo lỗi.
- Aggregate nằm trong `OVER (ORDER BY sum(x))` trước đây không được gom.
- Chỉ số của `quantileExactLow(p)` với p ≠ 0,5: ClickHouse lấy `⌊p·n⌋`.
- Khoá bị gộp bởi `WITH ROLLUP` của ClickHouse là giá trị mặc định của kiểu, không phải NULL.
- `string_agg(... ORDER BY ... DESC)` trước đây bỏ qua DESC.
- `length()` của mảng phải đếm phần tử.
- `BETWEEN SYMMETRIC` trước đây được chấp nhận nhưng bị bỏ qua.

Hai câu do bộ test viết sai đã được thay:
- `any(x)` của ClickHouse không tất định, vì phụ thuộc thứ tự quét.
- `FINAL` trên bảng `MergeTree` thường bị chính ClickHouse báo lỗi.

Test đơn vị và các bộ test cũ (`go test ./...`) đều pass.

## 10. TLS và SCRAM

Hạng mục P5.1 của lộ trình. Mã nguồn: `internal/pgwire/auth.go`, test ở `internal/pgwire/auth_test.go`.

### Cờ

| Cờ | Ý nghĩa |
|---|---|
| `-pg-tls-cert`, `-pg-tls-key` | File PEM của chứng chỉ và khoá. Phải đặt cùng nhau. Khi có, server trả lời `SSLRequest` bằng `S` rồi bắt tay TLS (tối thiểu TLS 1.2). |
| `-pg-tls-require` | Từ chối kết nối không mã hoá bằng FATAL `28000` (`no pg_hba.conf entry for host ..., no encryption`), giống `pg_hba.conf` chỉ có dòng `hostssl`. Cần có chứng chỉ. |
| `-pg-auth` | `trust`, `password` (cleartext) hoặc `scram`. Để rỗng thì giữ hành vi cũ: có `-pg-password` là `password`, không có là `trust`. `password` và `scram` đều cần `-pg-password`. |
| `-pg-password` | Mật khẩu chung cho mọi user. Với `scram`, server tạo verifier khi khởi động (salt ngẫu nhiên 16 byte, 4096 vòng PBKDF2, như `scram_iterations` mặc định của PostgreSQL) và không dùng lại mật khẩu gốc khi xác thực. |

Ví dụ:

```sh
staskd -pg-addr 0.0.0.0:5432 -pg-tls-cert server.crt -pg-tls-key server.key -pg-tls-require \
       -pg-auth scram -pg-password 'bí-mật'
psql "host=stask-host port=5432 user=app dbname=stask sslmode=require"
```

### Hành vi

**TLS.** Luồng theo tài liệu giao thức PostgreSQL:
- `SSLRequest`: trả `S` rồi bắt tay TLS. Sau đó `StartupMessage` và mọi dữ liệu đi qua kết nối TLS. Nếu client đã gửi thêm byte trước khi bắt tay (tấn công buffer stuffing, CVE-2021-23214), server báo `08P01` và đóng kết nối.
- Direct SSL của PostgreSQL 17 (`sslnegotiation=direct`): byte đầu là `0x16` (bản ghi handshake TLS), server bắt tay ngay. Client bắt buộc phải gửi ALPN `postgresql`. Không có ALPN thì server báo FATAL `08P01` qua kết nối TLS rồi đóng. Client gửi ALPN khác thì bắt tay thất bại.
- `GSSENCRequest` luôn được trả `N`. Client có thể chuyển sang `SSLRequest` hoặc kết nối thường.
- Gửi `SSLRequest`/`GSSENCRequest` khi đã ở trong TLS: FATAL `08P01`.
- Kết nối replication (`replication=database`, walsender) dùng chung đường khởi tạo, nên cũng được TLS và SCRAM như kết nối thường.

**SCRAM-SHA-256** (RFC 5802, RFC 7677), đúng chuỗi message của PostgreSQL: `AuthenticationSASL` (danh sách cơ chế) → `SASLInitialResponse` → `AuthenticationSASLContinue` → `SASLResponse` → `AuthenticationSASLFinal` → `AuthenticationOk`.
- Trên kết nối TLS, server mời `SCRAM-SHA-256-PLUS` trước `SCRAM-SHA-256`. `-PLUS` dùng channel binding `tls-server-end-point` (RFC 5929): băm chứng chỉ server bằng hàm băm của thuật toán chữ ký, MD5/SHA-1 thì đổi thành SHA-256.
- Client gửi cờ `y` (biết channel binding nhưng tưởng server không hỗ trợ) trong khi server đã mời `-PLUS`: coi là tấn công hạ cấp, báo `08P01`.
- Tên user trong `client-first-message` bị bỏ qua, server dùng user của startup packet (như PostgreSQL). `authzid` và extension `m=` bị từ chối.
- Sai mật khẩu: FATAL `28P01 password authentication failed for user "..."`. Message sai cấu trúc, nonce sai, channel binding sai: `08P01`.

**Mã lỗi:** `28P01` sai mật khẩu, `28000` kết nối bị chính sách từ chối (thiếu TLS), `08P01` vi phạm giao thức.

### Chưa hỗ trợ và giới hạn

- **SASLprep:** server băm mật khẩu ở dạng byte gốc. Với mật khẩu ASCII không có ký tự điều khiển, SASLprep là phép đồng nhất, nên kết quả trùng PostgreSQL. Mật khẩu ngoài ASCII mà SASLprep có ánh xạ (khoảng trắng không phải ASCII, ký tự cần chuẩn hoá NFKC, ký tự bị xoá) có thể lệch với client đã chuẩn hoá. Thư viện chuẩn của Go không có NFKC. Mật khẩu UTF-8 đã ở dạng NFC và không có ký tự đặc biệt (như `bí-mật`) vẫn dùng được, test có kiểm.
- **SCRAM-SHA-256-PLUS** chỉ được mời khi cấu hình có đúng một chứng chỉ và chứng chỉ ký bằng RSA/ECDSA/DSA với SHA-1/256/384/512. Chứng chỉ Ed25519 không có hàm băm cho `tls-server-end-point`, nên chỉ mời `SCRAM-SHA-256` (PostgreSQL cũng không hỗ trợ binding cho loại này). pgx v5.7.1 mà dự án đang dùng chưa biết `-PLUS` và chọn `SCRAM-SHA-256`. `-PLUS` được kiểm bằng một client tự viết trong test.
- Không có xác thực bằng chứng chỉ client (`clientcert`, `cert`), GSSAPI/Kerberos, `md5`, LDAP.
- Chưa có user/role: một mật khẩu chung cho mọi tên user, không có `pg_hba.conf` theo host/user/database.
- Chứng chỉ được nạp một lần khi khởi động. Đổi chứng chỉ phải khởi động lại.
- Bắt tay TLS có hạn 10 giây.

### Tài liệu tham khảo

- Luồng giao thức, `SSLRequest`, `GSSENCRequest`, direct SSL và ALPN: https://www.postgresql.org/docs/16/protocol-flow.html, https://www.postgresql.org/docs/17/protocol-flow.html (§53.2.10 "SSL Session Encryption")
- SASL/SCRAM trong PostgreSQL: https://www.postgresql.org/docs/16/sasl-authentication.html
- RFC 5802 (SCRAM): https://www.rfc-editor.org/rfc/rfc5802
- RFC 7677 (SCRAM-SHA-256): https://www.rfc-editor.org/rfc/rfc7677
- RFC 5929 (`tls-server-end-point`): https://www.rfc-editor.org/rfc/rfc5929
- RFC 7301 (ALPN): https://www.rfc-editor.org/rfc/rfc7301

## 11. ALTER TABLE (2026-10-29)

Đã hỗ trợ:

- `ADD COLUMN [IF NOT EXISTS]` với `DEFAULT`, `NOT NULL`, `CHECK`.
- `DROP COLUMN [IF EXISTS]`.
- `RENAME COLUMN`, `RENAME TO`.
- `ALTER COLUMN SET/DROP DEFAULT`, `SET/DROP NOT NULL`.
- `ADD [CONSTRAINT x] CHECK`, `DROP CONSTRAINT [IF EXISTS]`.
- `OWNER TO`, được chấp nhận nhưng bỏ qua.
- Nhiều action trong một câu, áp dụng nguyên tử.

Cách làm giống PostgreSQL, không ghi lại bảng. Định dạng chi tiết ở docs/09 §5.

- **ADD COLUMN**: DEFAULT được tính một lần lúc ALTER, rồi các dòng cũ đọc ra giá trị đó (`attmissingval` của [PG 11+](https://www.brandur.org/postgres-default)). DEFAULT "volatile" (`nextval`, `random()`, `gen_random_uuid()`…) bị từ chối với `0A000`, vì mỗi dòng cũ sẽ cần một giá trị riêng. PG chấp nhận trường hợp này bằng cách ghi lại cả bảng.
- **DROP COLUMN**: chỉ đánh dấu cột là đã xoá (`attisdropped` của [PG](https://www.postgresql.org/docs/current/sql-altertable.html)). Dòng cũ giữ giá trị đó cho tới khi được ghi lại hoặc bị GC dời đi, và bộ giải mã bỏ qua nó. Tên cột được giải phóng, nên có thể ADD lại cùng tên với kiểu khác. Đi kèm với cột bị xoá:
  - CHECK nhắc tới cột bị xoá theo, như PG.
  - Sequence của cột SERIAL/IDENTITY bị xoá theo.
  - View cột bỏ vector của cột đó.
- **SET NOT NULL** và **ADD CHECK** kiểm tra các dòng đang có. Có dòng vi phạm thì trả `23502` hoặc `23514`.
- **RENAME**: publication và sequence sở hữu đi theo tên mới. RENAME COLUMN bị từ chối nếu một CHECK nhắc tới cột (vì CHECK lưu dạng văn bản).
- **Ghi đồng thời**: một câu lệnh có dòng được dựng theo schema cũ sẽ bị xung đột, và autocommit tự làm lại với schema mới. Giao dịch tường minh nhận `40001`.

Hạn chế, kèm cách xử lý:

| Hạn chế | Xử lý |
|---|---|
| `ALTER COLUMN TYPE` (`0A000`) | Hoãn sang P1.2b: cần ghi lại các dòng ở nền, dùng chính cơ chế bố cục theo LSN |
| DROP COLUMN trên bảng time-series (`0A000`), hoặc chính cột đang được view search đánh chỉ mục | Hoãn: view time-series lưu block theo vị trí cột. Bảng time-series là append-only nên hiếm khi bị đổi. Các cột khác của bảng có search thì xoá được |
| DROP COLUMN cột khoá chính (`0A000`) | Không làm, vì bảng STASK luôn cần khoá chính. PG cho phép và bỏ luôn ràng buộc |
| Cột thêm bằng ADD COLUMN không được lưu trong view cột cho tới lần khởi động sau (aggregate trên cột đó chạy trên dòng) | Hoãn: dựng vector ở nền |
| SET NOT NULL và ADD CHECK kiểm tra trên snapshot. Một dòng vi phạm được ghi đúng lúc ALTER đang kiểm tra có thể lọt | Hoãn tới khi có khoá bảng (P5.2). PG dùng `ACCESS EXCLUSIVE` |
| Không có `ALTER TABLE ... ADD PRIMARY KEY/UNIQUE/FOREIGN KEY` | Theo P1 (UNIQUE) và P3 (khoá ngoại) |

## 12. Ngân sách bộ nhớ truy vấn (2026-10-29)

Bộ thực thi tổng quát (JOIN, subquery, hàm trả tập) giữ các dòng trong RAM và chưa spill ra đĩa. Trước đây, một JOIN lớn có thể làm cả tiến trình OOM. Giờ mỗi dòng được tính vào hai ngân sách lúc dựng ra, giống `--max-sql-memory` của [CockroachDB](https://www.netdata.cloud/guides/cockroachdb/cockroachdb-sql-memory-budget-53200/):

| Cờ | Mặc định | Ý nghĩa |
|---|---|---|
| `-sql-mem-mb` | ¼ giới hạn heap (`-mem-limit-mb`, mặc định ½ RAM) | Tổng bộ nhớ các truy vấn đang chạy được giữ. `-1` = không giới hạn |
| `-query-mem-mb` | ½ của `-sql-mem-mb` | Bộ nhớ một truy vấn được giữ. `-1` = không giới hạn |

- Vượt ngân sách thì truy vấn hỏng với `53200 out of memory`, còn các truy vấn khác và server không bị ảnh hưởng.
- Phần dành sẵn được lấy theo từng bước 1 MB (một phép atomic trên mỗi MB, không phải trên mỗi dòng) và trả lại khi truy vấn kết thúc.
- Không áp dụng cho đường nhanh (đọc theo khoá, aggregate trên view cột), vì chúng không dựng dòng vào RAM.

Hạn chế: kích thước được ước lượng theo `Datum` và độ dài chuỗi (ước lượng thấp hơn thực tế khoảng 25%, đo ở §12.1). Khi bật spill (mặc định), truy vấn vượt ngân sách không còn hỏng ngay mà chạy lại ở chế độ tràn đĩa, xem §12.1.

### 12.1 Tràn xuống đĩa (P2.4, 2026-10-29)

Mã nguồn: `internal/spill` (toán tử, không biết gì về SQL), `internal/sql/spillexec.go` (nối vào bộ thực thi), `cmd/staskd/main.go` (cờ). Test: `internal/spill/spill_test.go`, `internal/sql/spillexec_test.go`, `internal/sql/spillbig_test.go`.

**Cách các hệ khác làm** (tham khảo, đọc theo trí nhớ về thiết kế, chưa kiểm lại từng trang trong phiên này): PostgreSQL giới hạn mỗi toán tử bằng `work_mem` (hash còn nhân `hash_mem_multiplier`), vượt thì sort ghi các run ra file tạm rồi trộn ngoài (`tuplesort.c`), hash join chia thành nhiều batch trên đĩa (`nodeHash.c`), tổng file tạm của một session bị chặn bởi `temp_file_limit` ([docs](https://www.postgresql.org/docs/current/runtime-config-resource.html)). DuckDB tách aggregate theo phân vùng radix, phân vùng nào không vừa thì ghi ra file tạm ([external aggregation](https://duckdb.org/2024/03/29/external-aggregation.html), [quản lý bộ nhớ](https://duckdb.org/2024/07/09/memory-management.html)). ClickHouse đặt ngưỡng riêng cho từng toán tử: `max_bytes_before_external_group_by`, `max_bytes_before_external_sort`, và `join_algorithm = grace_hash` ([settings](https://clickhouse.com/docs/operations/settings/settings)). STASK theo mô hình chung của ba hệ: mỗi toán tử có ngưỡng RAM riêng, vượt ngưỡng mới chạm đĩa, file tạm có hạn mức.

**Cơ chế.** Truy vấn chạy bình thường trước. Nếu gặp `53200` và `SpillDir` được đặt, truy vấn được chạy lại **một lần** ở chế độ tràn đĩa (`runPlan`), nên truy vấn vừa RAM không tốn gì thêm và không có đường mã mới nào chạy cho chúng. Ở chế độ này, một `SELECT` thuộc các dạng nêu dưới đây được chạy như chuỗi nguồn đẩy (scan, hash join, lọc, aggregate, chiếu, DISTINCT, ORDER BY, LIMIT) thay vì các bảng quan hệ dựng sẵn trong RAM. Scan không còn dựng cả bảng vào RAM.

| Toán tử | Cách tràn | Giới hạn RAM |
|---|---|---|
| ORDER BY | Sort ngoài: sắp từng run trong RAM, ghi run ra file, trộn bằng heap (tối đa 64 file một lượt, nhiều hơn thì trộn nhiều lượt). Ổn định (đồng khoá giữ thứ tự vào) | `-spill-op-mb` |
| JOIN (inner, left, right, full, có residual) | Grace hash join: build bên phải vào bảng băm; vượt ngưỡng thì cả hai bên chia thành 32 phân vùng theo hash của khoá, mỗi cặp phân vùng join riêng, phân vùng build vẫn quá lớn thì chia tiếp (tối đa 4 mức, hash khác mỗi mức). Dòng có khoá NULL không bao giờ khớp nhưng vẫn được báo cho outer join | `-spill-op-mb` |
| GROUP BY | Hash aggregate: nhóm đã có trong bảng tiếp tục được cộng dồn; dòng của nhóm mới khi bảng đã đầy ghi vào 32 phân vùng, xử lý đệ quy sau khi xả bảng. Nhóm không bao giờ bị tách làm hai | `-spill-op-mb` |
| DISTINCT | Cùng cơ chế với GROUP BY | `-spill-op-mb` |

Dạng được hỗ trợ: không có hàm cửa sổ, hàm trả tập trong select list, `DISTINCT ON`, `LIMIT BY`, grouping sets/ROLLUP; aggregate chỉ gồm loại có trạng thái cố định (`count sum avg min max bool_* stddev/variance any anyLast argMin argMax` và các biến thể `-If`, không `DISTINCT`, không `ORDER BY` trong lời gọi). Dạng khác chạy như cũ và vẫn hỏng `53200` khi vượt ngân sách. Join không có điều kiện bằng thì giữ vế phải trong RAM (tính vào ngân sách) và quét vế trái dạng luồng. Join LATERAL, hàm bảng, CTE, subquery trong FROM vẫn là quan hệ dựng sẵn (bị tính vào ngân sách). Kết quả cuối cùng cũng nằm trong RAM (giao thức trả `Result.Rows`) và bị tính vào ngân sách: truy vấn trả hàng chục triệu dòng vẫn hỏng `53200`.

**File tạm.** Mỗi truy vấn có thư mục riêng `<spill-dir>/stask-spill-<pid>-<n>`. File được mở rồi `unlink` ngay, nên mọi lối thoát (lỗi, huỷ, panic, tiến trình chết) đều không để lại dữ liệu; thư mục bị xoá khi truy vấn xong dù thành công hay lỗi. Khi khởi động, `staskd` xoá mọi thư mục `stask-spill-*` còn sót trong `-spill-dir`. Mỗi bản ghi có độ dài + CRC-32C; đọc lại sai CRC hoặc cụt thì truy vấn hỏng `58030`. Hạn mức dung lượng một truy vấn `-spill-max-mb` (vượt thì `53100`). Huỷ câu lệnh và `statement_timeout` được kiểm tra mỗi 2048 bản ghi ghi/đọc và trong vòng lặp join/lọc, trả `57014` như thường.

| Cờ | Mặc định | Ý nghĩa |
|---|---|---|
| `-spill` | `true` | Tắt (`false`) thì giữ hành vi cũ: vượt ngân sách là `53200` |
| `-spill-dir` | `<dir>/spill` | Thư mục file tạm (nên cùng ổ nhanh, khác ổ với log nếu có) |
| `-spill-max-mb` | 0 (không giới hạn) | Dung lượng file tạm tối đa của một truy vấn |
| `-spill-op-mb` | 0 = `-query-mem-mb` / 6 | RAM mỗi toán tử trước khi tràn; phần này được giữ chỗ trong ngân sách của truy vấn |

**Hạn chế đã biết.** (1) Truy vấn tràn đĩa làm việc đến lúc hết ngân sách một lần rồi làm lại; hàm có tác dụng phụ trong SELECT (như `nextval`) có thể chạy hai lần. Có thể tránh bằng ước lượng kích thước trước từ số dòng của bảng, chưa làm. (2) Đường nhanh một bảng (`selectRows`: `SELECT ... FROM t GROUP BY/ORDER BY` không join) không đi qua ngân sách này nên cũng chưa có spill. (3) Ước lượng bộ nhớ thấp hơn thực tế; khoá lệch (một khoá có nhiều dòng hơn RAM của toán tử) sau 4 mức chia thì hỏng `53200` thay vì chạy tiếp. (4) Hash join luôn build bên phải và chưa giữ phân vùng đầu trong RAM (hybrid đúng nghĩa), nên khi tràn thì ghi cả hai bên ra đĩa. (5) Tổng RAM nhiều toán tử lồng nhau có thể lên tới ba phần sáu ngân sách. (6) Sort chưa có đường top-k cho `ORDER BY ... LIMIT`. (7) Aggregate có trạng thái tăng theo nhóm (`array_agg`, `string_agg`, `uniq`, quantile, `DISTINCT` trong lời gọi) chưa tràn. (8) Cộng số thực theo thứ tự khác nên kết quả `sum/avg` của double có thể khác ở chữ số cuối so với chạy trong RAM (test so sánh sau khi làm tròn 9 chữ số).

## 13. Huỷ câu lệnh, timeout của session, trần tuổi snapshot (2026-10-29)

Hạng mục P5.3 và M2, M3, C4 của docs/12. Mã nguồn: `internal/sql/cancel.go` (trạng thái câu lệnh, `SET`, parse thời gian), `internal/pgwire/pgwire.go` (CancelRequest, timeout lúc rảnh, keepalive), `internal/pgwire/sockopt_linux.go` (`TCP_USER_TIMEOUT`), `internal/core/gc.go` (trần tuổi snapshot), `internal/core/tx.go` và `engine.go` (điểm kiểm tra trong scan), `internal/proj/columnar.go` (kiểm tra theo chunk trong aggregate dạng cột). Test: `internal/pgwire/cancel_test.go`, `internal/sql/cancel_test.go`, `internal/core/snapage_test.go`, `internal/proj/interrupt_test.go`.

### Cờ và biến session

| Cờ của `staskd` (cũng dùng được trong file `-config`) | Biến `SET`/`SHOW` | Mặc định | Ý nghĩa |
|---|---|---|---|
| `-statement-timeout` | `statement_timeout` | `0` (tắt) | Câu lệnh chạy lâu hơn bị huỷ: `ERROR 57014 canceling statement due to statement timeout` |
| `-idle-in-transaction-session-timeout` | `idle_in_transaction_session_timeout` | `0` | Session rảnh **trong** `BEGIN` lâu hơn thì bị đóng: `FATAL 25P03 terminating connection due to idle-in-transaction timeout`. Giao dịch bị huỷ, snapshot được bỏ ghim |
| `-idle-session-timeout` | `idle_session_timeout` | `0` | Session rảnh **ngoài** giao dịch lâu hơn thì bị đóng: `FATAL 57P05 terminating connection due to idle-session timeout` |
| `-max-snapshot-age` | — | `24h` | Snapshot cũ (đã có commit mới hơn) giữ quá lâu thì thôi chặn horizon của GC; giao dịch giữ nó nhận `72000 snapshot is older than the retained history` ở lần đọc hoặc commit kế tiếp. `0` = không giới hạn |
| `-pg-tcp-keepalives-idle` | — | `1m` | Như `tcp_keepalives_idle`. `0` = mặc định của hệ điều hành |
| `-pg-tcp-keepalives-interval` | — | `10s` | Như `tcp_keepalives_interval` |
| `-pg-tcp-keepalives-count` | — | `6` | Như `tcp_keepalives_count`. Với ba giá trị mặc định, client chết khi đang rảnh bị phát hiện sau khoảng 2 phút |
| `-pg-tcp-user-timeout` | — | `0` | Như `tcp_user_timeout` (chỉ Linux, `TCP_USER_TIMEOUT`, RFC 5482): dữ liệu đã gửi mà không được ACK quá thời gian này thì kernel đóng kết nối. `0` = mặc định hệ điều hành (gửi lại khoảng 15 phút) |

Giá trị thời gian theo cú pháp PostgreSQL: số không có đơn vị là mili giây, hoặc kèm `us`, `ms`, `s`, `min`, `h`, `d` (`'5s'`, `1500`, `'1.5 min'`). `SHOW` in như PostgreSQL (`5s`, `1500ms`, `90min`, `0`). `SET x TO DEFAULT` trả về giá trị mặc định của server. Giá trị âm báo `22023`. Cờ `-max-snapshot-age` và các cờ `-pg-tcp-*` dùng cú pháp duration của Go (`24h`, `30s`).

### Hành vi

**CancelRequest.** `BackendKeyData` gửi khoá bí mật 32 bit lấy từ `crypto/rand` cho mỗi session (trước đây là `time.Now()`, đoán được). Kết nối huỷ đọc pid và khoá, so khoá bằng `subtle.ConstantTimeCompare`, rồi đóng kết nối mà không trả lời gì (đúng giao thức, để không dò được cặp pid/khoá). Khoá đúng và session đang chạy một câu lệnh thì câu lệnh đó dừng với `ERROR 57014 canceling statement due to user request`. Session vẫn dùng tiếp được. Trong `BEGIN`, lỗi này làm giao dịch vào trạng thái lỗi (`25P02` cho lệnh sau, tới `ROLLBACK`), giống PostgreSQL. Khoá sai, pid không tồn tại, hoặc session đang rảnh: không có tác dụng gì.

**Cơ chế dừng.** Mỗi session có một từ atomic `seq<<3 | đang_chạy<<2 | lý_do`. `Exec` ghi seq mới khi bắt đầu câu lệnh. Cancel và timer của `statement_timeout` chỉ đặt lý do bằng CAS khi đúng câu lệnh đó còn đang chạy. Vì vậy timer đến muộn, hoặc cancel đến lúc session rảnh, không ảnh hưởng câu lệnh sau. Các vòng lặp dài đọc lý do bằng một atomic load:
- scan tuần tự của giao dịch (`Tx.ScanAll`): mỗi 256 dòng;
- scan song song (`ScanParallel` qua `Tx.ScanAllParallel`): trước mỗi chunk 2048 dòng và mỗi 256 dòng trong chunk;
- aggregate trên view dạng cột: trước mỗi chunk 65 536 dòng ở mọi worker (`AggQuery.Interrupted`), kết quả dở dang bị bỏ;
- bộ thực thi tổng quát: vòng join (mỗi 4096 cặp) và lọc `WHERE` (mỗi 4096 dòng).

Chi phí mỗi dòng gần như bằng 0: một phép AND và một nhánh, cứ 256 dòng mới gọi một hàm. Mỗi câu lệnh tốn thêm hai lần ghi atomic. Chỉ khi bật `statement_timeout` mới thêm một `time.AfterFunc`.

**Kết quả đo** (VM 8 vCPU, `go test`): aggregate quét toàn bảng 300k dòng (`md5(repeat(s, 500))` mỗi dòng, chạy vài giây), gửi `CancelRequest` qua pgx sau 400 ms. Câu lệnh trả `57014` sau 6–35 ms (5 lần chạy), dù cả 8 worker scan đang chiếm hết CPU, và client lẫn server chạy chung một tiến trình. Gọi thẳng ở tầng SQL: 1–9 ms, cả tuần tự lẫn 4 worker. Lần đầu chỉ kiểm tra theo chunk thì mất 37–87 ms, vì mỗi chunk 2048 dòng × ~10 µs/dòng tốn khoảng 20 ms. Do đó mới thêm điểm kiểm tra mỗi 256 dòng trong chunk. Tiêu chí P5.3 (dưới 100 ms) đạt.

**Timeout lúc rảnh.** Server đặt read deadline trên socket khi chờ message đầu tiên sau `ReadyForQuery`. Đang ở giữa một chuỗi extended protocol (trước `Sync`) thì không tính là rảnh, như PostgreSQL. Có byte đầu tiên đến là deadline bị xoá. Quá hạn thì server gửi FATAL rồi đóng session. Lúc đóng, giao dịch bị abort và snapshot bỏ ghim, nên horizon của GC tiến tiếp ngay. Test kiểm `PinnedSnapshots` về 0 và `horizon_lsn` bằng durable LSN.

**Trần tuổi snapshot.** Mỗi pin của giao dịch lưu thời điểm pin gần nhất của LSN đó. Giao dịch luôn pin durable LSN, nên LSN nhỏ hơn thì lần pin cuối cũng sớm hơn. Khi tính horizon, pin nào đủ hai điều kiện sau thì bị coi là hết hạn:
1. LSN < durable LSN, tức snapshot đã cũ và không giao dịch mới nào pin lại được nó. Snapshot vẫn là durable point thì không chặn gì, nên không bao giờ hết hạn, dù để lâu bao nhiêu.
2. Lần pin cuối đã quá `-max-snapshot-age`.

Khi đó `snapshot_cut_lsn` được nâng qua LSN của pin, *trước khi* pin thôi được tính. Mọi `Get`/`Scan`/`ScanAll`/`Commit` của giao dịch có snapshot dưới mốc này trả `ErrSnapshotTooOld` (SQLSTATE `72000`, cùng lỗi với đọc as-of dưới floor), thay vì đọc thiếu phiên bản đã bị thu hồi. Đọc đang chạy dở vẫn an toàn nhờ read epoch có sẵn: segment chỉ bị xoá sau `waitReaders`. Pin của slot CDC và follower nằm ở shard riêng, không bị trần này cắt. Slot đã có `-slot-max-lag-mb`. `/v1/status` (khối `gc`) có thêm `oldest_snapshot_age_ms` (snapshot cũ nhất còn chặn horizon đã bị giữ bao lâu), `snapshot_cut_lsn` và `max_snapshot_age`.

Mặc định `24h`, vì:
- (a) nó đặt một trần cho dung lượng log mà một session bỏ quên có thể giữ lại, trong khi `idle_in_transaction_session_timeout` mặc định tắt như PostgreSQL;
- (b) bằng mặc định 24 h của `tidb_gc_max_wait_time` ở TiDB, cơ chế giống hệt (ép safe point vượt giao dịch giữ quá lâu);
- (c) đủ dài để báo cáo/ETL chạy nhiều giờ không bị cắt.

Hệ thống muốn chặt hơn thì đặt `idle_in_transaction_session_timeout` (ví dụ `10min`) và hạ `-max-snapshot-age`.

**Keepalive.** Mỗi kết nối PG được đặt `SetKeepAliveConfig` (idle/interval/count). Trên Linux có thêm `TCP_USER_TIMEOUT` nếu được cấu hình. Keepalive phát hiện client chết khi kết nối đang rảnh. `TCP_USER_TIMEOUT` phát hiện client chết khi server đang gửi kết quả mà không nhận được ACK. Session bị đóng thì giao dịch cũng bị abort và snapshot được bỏ ghim.

### Giới hạn và cách xử lý

| Giới hạn | Xử lý |
|---|---|
| `statement_timeout` tính cho từng câu lệnh. PostgreSQL tính cho cả chuỗi lệnh trong một message `Query`, hoặc từ `Parse` tới `Sync` | Không làm: chênh lệch chỉ lộ ra với chuỗi nhiều lệnh, mỗi lệnh vẫn bị chặn |
| Chưa dừng được giữa chừng: sắp xếp, hàm cửa sổ, gom nhóm trên dữ liệu đã nằm trong RAM của bộ thực thi tổng quát; chờ view dạng cột bắt kịp (`waitView`, tối đa 10 s); commit | Hoãn sang P2 (bộ thực thi có spill, cũng cần điểm kiểm tra). Phần quét bảng, nơi tốn thời gian nhất, đã dừng được. Commit thì PostgreSQL cũng không huỷ giữa chừng |
| Kết nối walsender (CDC) không nhận cancel cho lệnh replication | Không làm: client đóng kết nối là dừng stream |
| Khoá bí mật 32 bit (giao thức 3.0). PostgreSQL 18 với giao thức 3.2 cho khoá dài tới 256 byte | Hoãn tới khi hỗ trợ đàm phán giao thức 3.2 |
| Trần tuổi snapshot chỉ được xét khi tính horizon (mỗi vòng GC theo `-gc-interval`, hoặc khi đọc `/v1/status`), nên hạn thực tế là `max-snapshot-age` cộng tối đa một vòng GC | Chấp nhận: sai số 10 s so với hạn tính bằng giờ |
| Chưa đo bằng benchmark lớn chi phí của 2 atomic mỗi câu lệnh và 1 atomic load mỗi `Get` | Đo cùng lần chạy `staskmix` kế tiếp. Dự kiến không thấy được (dưới 1 ns so với ~10 µs mỗi câu lệnh) |

### Tài liệu tham khảo

- Huỷ yêu cầu đang chạy (CancelRequest, BackendKeyData, không trả lời): [PostgreSQL — Protocol Flow §Canceling Requests in Progress](https://www.postgresql.org/docs/current/protocol-flow.html#PROTOCOL-FLOW-CANCELING-REQUESTS)
- `statement_timeout`, `idle_in_transaction_session_timeout`, `idle_session_timeout`: [PostgreSQL — Client Connection Defaults](https://www.postgresql.org/docs/current/runtime-config-client.html)
- SQLSTATE `57014`, `25P03`, `57P05`: [PostgreSQL — Error Codes](https://www.postgresql.org/docs/current/errcodes-appendix.html)
- `tcp_keepalives_*`, `tcp_user_timeout`: [PostgreSQL — Connections and Authentication](https://www.postgresql.org/docs/current/runtime-config-connection.html); [RFC 5482 (TCP User Timeout)](https://www.rfc-editor.org/rfc/rfc5482); [Cloudflare — When TCP sockets refuse to die](https://blog.cloudflare.com/when-tcp-sockets-refuse-to-die/); [Go `net.KeepAliveConfig`](https://pkg.go.dev/net#KeepAliveConfig)
- PostgreSQL bỏ `old_snapshot_threshold` ở bản 17 vì lỗi đúng đắn: [dbi services — No more snapshot too old in PostgreSQL 17](https://www.dbi-services.com/blog/no-more-snapshot-too-old-in-postgresql-17/), [commitfest: Remove old_snapshot_threshold](https://commitfest.postgresql.org/43/4363). STASK không lặp lại lỗi đó vì không xoá phiên bản theo thời gian một cách lặng lẽ: giao dịch quá hạn luôn nhận lỗi rõ ràng trước khi horizon vượt qua snapshot của nó.
- TiDB: [GC safe point và `tidb_gc_max_wait_time`](https://docs.pingcap.com/tidb/dev/garbage-collection-configuration/), [timeout trong TiDB](https://docs.pingcap.com/developer/dev-guide-timeouts-in-tidb/), [TiDB 6.1.0 release notes (mặc định 24 h)](https://docs.pingcap.com/tidb/v8.3/release-6.1.0)
- CockroachDB: [GC threshold, `gc.ttlseconds`, protected timestamps](https://www.cockroachlabs.com/blog/protected-timestamps-for-less-garbage/)
- MySQL/InnoDB: giao dịch dài làm purge tụt lại, `innodb_max_purge_lag`: [MySQL — Purge Configuration](https://dev.mysql.com/doc/refman/8.0/en/innodb-purge-configuration.html)

## 14. Backup nóng và khôi phục tới một thời điểm (PITR) (2026-10-29)

Giống [lưu trữ liên tục của PostgreSQL](https://www.postgresql.org/docs/current/continuous-archiving.html), nhưng dùng thẳng log của STASK: mỗi bản ghi đã mang LSN và thời điểm commit nên không cần file lịch sử hay manifest. Mã: `internal/core/archive.go`.

| Bước | Cờ staskd | Việc làm |
|---|---|---|
| 1. Lưu trữ liên tục | `-archive-dir D` | Mỗi segment log đã đóng được chép sang `D/NNNNNNNN.wal` (ghi dưới tên tạm, fsync, đổi tên). GC không xoá segment nào trước khi nó được lưu, nên kho chứa mọi bản ghi kể từ lúc bật |
| 2. Base backup | `-backup-from ADDR -dir B` | Lấy checkpoint và log sau nó từ leader (cổng nhân bản `-repl-addr`, dùng các cờ `-repl-*` về TLS/secret) vào `B`, rồi thoát |
| 3. Khôi phục | `-dir B -restore-archive D [-recovery-target-time T \| -recovery-target-lsn N]` | Chép các segment `B` còn thiếu từ `D`, phát lại tới mục tiêu, bỏ phần log phía sau, engine chạy tiếp từ đó |

- `-recovery-target-time` (RFC 3339): dừng trước commit đầu tiên sau thời điểm đó. `-recovery-target-lsn`: dừng sau LSN đó. Không có mục tiêu thì phát lại hết kho.
- Khôi phục được tới mọi điểm sau checkpoint của base backup. Muốn về điểm trước đó cần base backup cũ hơn.
- `-archive-dir` của DB đã khôi phục phải là thư mục mới (log sau mục tiêu khác log đã lưu); staskd từ chối nếu trùng `-restore-archive`.
- Mục tiêu mà không có `-restore-archive` bị từ chối.

Kiểm tra: `TestPointInTimeRecovery` (`internal/core/archive_test.go`) qua trên VM 2026-10-29: lưu 3 segment, khôi phục tới LSN và tới thời điểm, kết quả đúng. **Chưa kiểm tra:** chạy `-backup-from` qua mạng với TLS/mTLS trên staskd thật, kho lưu trữ lớn (hàng chục GB), và khôi phục khi leader đang ghi nặng. Cần làm trước khi coi P5.4 là xong.

## 15. COPY (2026-10-29)

Hạng mục P1.5 (phần COPY) của docs/12. Mã nguồn: `internal/sql/copy.go` (parse, chuyển dòng thành hàng và ngược lại, ngân sách bộ nhớ), `internal/pgwire/copy.go` (CopyInResponse `G`, CopyOutResponse `H`, CopyData `d`, CopyDone `c`, CopyFail `f`). Test: `internal/pgwire/copy_test.go`. Câu lệnh được nhận diện bằng `sql.ParseCopy` ngay trước `sql.Parse` trong `simpleQuery`, nên ngữ pháp SQL không đổi.

**Cú pháp.** `COPY tbl [(cột, ...)] FROM STDIN`, `COPY tbl [(cột, ...)] TO STDOUT`, `COPY (SELECT ...) TO STDOUT`, kèm `[WITH] (FORMAT text|csv, DELIMITER, NULL, HEADER, QUOTE, ESCAPE, ENCODING 'utf8')` hoặc kiểu cũ (`CSV HEADER`, `DELIMITER 'x'`, `NULL 'x'`...). Mặc định như PostgreSQL: text dùng tab và `\N`, csv dùng `,`, NULL là chuỗi rỗng không ngoặc, chuỗi rỗng có ngoặc là chuỗi rỗng. Text hiểu `\b \f \n \r \t \v \\`, `\ooo`, `\xHH` và dòng `\.` cuối dữ liệu. Csv cho phép xuống dòng trong ngoặc. `HEADER` khi nạp bỏ qua dòng đầu (không kiểm tên cột, kể cả `MATCH`), khi xuất ghi tên cột. Cột không có trong danh sách nhận `DEFAULT` (kể cả `nextval`).

**Từ chối rõ ràng.** `BINARY` / `FORMAT binary`: `0A000`. Lưu ý `pgx.CopyFrom` luôn dùng binary nên bị từ chối; dùng `conn.PgConn().CopyFrom` với câu `COPY ... FROM STDIN`, hoặc `psql \copy`. `FROM/TO 'file'` và `PROGRAM`: `0A000` (chỉ STDIN/STDOUT). `FORCE_QUOTE`, `FORCE_NOT_NULL`, `FORCE_NULL`, `ON_ERROR`, `DEFAULT`...: `0A000`. Option sai: `22023`. COPY qua giao thức mở rộng (Parse/Bind) chưa có: chỉ simple query (psql, JDBC `CopyManager`, `PgConn().CopyFrom` đều dùng simple query).

**Nạp.** Toàn bộ `COPY FROM` là một giao dịch: trong `BEGIN` thì dùng giao dịch đó (lỗi làm khối giao dịch ở trạng thái aborted, như PostgreSQL), ngoài `BEGIN` thì tự mở giao dịch, commit ở CopyDone và huỷ ở mọi lỗi hoặc CopyFail, nên lỗi giữa chừng không để lại dòng nào. Dữ liệu được xử lý theo từng CopyData (không giữ cả luồng); chỉ phần dòng dở được giữ lại, trần 256 MB một dòng (`54000`). Lỗi có ngữ cảnh `COPY tbl, line N, column c`. Mã lỗi: `22P02` sai giá trị, `22P04` thiếu hoặc thừa cột, `23502` NOT NULL, `23505` trùng khoá (với bảng, trong cùng luồng hoặc với dòng đã có), `22021` UTF-8 sai. Mỗi hàng nạp được tính vào ngân sách bộ nhớ của §12 (`-query-mem-mb`, `-sql-mem-mb`: vượt thì `53200` và rollback, phần đã giữ được trả lại). `statement_timeout` tính cho cả câu COPY kể cả lúc chờ client, và CancelRequest có hiệu lực ở CopyData kế tiếp (`57014`). Khi đang COPY, timeout rảnh (`idle_*`) không áp dụng.

**Xuất.** `COPY tbl TO STDOUT` quét bảng theo luồng trên một snapshot (không giữ cả bảng trong RAM; thứ tự không xác định) và gửi mỗi dòng một CopyData. `COPY (SELECT ...)` chạy truy vấn bằng bộ thực thi thường (kết quả nằm trong RAM, chịu ngân sách §12) rồi gửi. Bảng `columnar` nạp và xuất như bảng thường; view cột được dựng từ log nên aggregate sau COPY đúng (có test so với tính tay).

**Kiểm chứng** (`go test ./internal/pgwire -run Copy`, cả `-race`): round trip text và csv (NULL, chuỗi rỗng, `\N` là văn bản, tab, xuống dòng, backslash, dấu phẩy, ngoặc kép, CRLF), danh sách cột, DEFAULT, dấu phân cách và NULL tuỳ chọn, dòng `\.`; lỗi giữa 20 000 dòng (sai kiểu, trùng khoá trong luồng và với dòng có sẵn, NOT NULL, thiếu/thừa cột, CopyFail của client) để lại bảng nguyên vẹn; trong `BEGIN` (lỗi, ROLLBACK, COMMIT); các từ chối ở trên; bảng columnar 50 000 dòng; `statement_timeout` giữa luồng; ngân sách bộ nhớ.

**Đo thử** (`STASK_COPY_ROWS=1000000 go test ./internal/pgwire -run CopyVsInsert -v`; 4 cột bigint/bigint/text/double, qua pgx và TCP loopback, máy dev đang có tải khác nên chỉ so sánh tương đối, mỗi con số là một lần chạy): 1 000 000 dòng, `COPY` mất 4,6 s và 6,2 s (hai lần; 215 và 160 nghìn dòng/s), `INSERT` 1000 dòng/câu trong một giao dịch mất 9,8 s và 10,2 s (khoảng 100 nghìn dòng/s). Ở 100 000 dòng: 0,52 s so với 0,84 s. Một bản đầu cấp phát lại mảng ghi của giao dịch theo từng 4096 dòng (`Tx.Grow` cấp đúng cỡ), chậm hơn INSERT ở 1M (14 s so với 8,6 s); đã sửa bằng tăng gấp đôi. Chưa đo 10M (tiêu chí của P1.5 là 10M; ngoại suy chưa phải số đo), chưa đo INSERT từng câu autocommit và chưa so với `COPY` của PostgreSQL.

**Chưa làm.** Binary; file/program; `FORCE_*`; `HEADER MATCH` kiểm tên cột; COPY qua giao thức mở rộng; `COPY` trong khối nhiều câu (`COPY ...; SELECT ...` trong một câu Query); cả giao dịch 10M dòng nằm trong RAM tới lúc commit (chưa chia lô commit, nên bộ nhớ phụ thuộc kích thước nạp; ngân sách chỉ báo lỗi chứ chưa spill); `COPY TO` từ truy vấn chưa phát theo luồng; COPY chưa qua hàng đợi admission (`lanes.go`); chưa thử bằng `psql \copy` thật (không có psql trên máy test), chỉ bằng pgx.

## 16. CREATE VIEW và DROP VIEW (2026-10-29)

Cú pháp: `CREATE [OR REPLACE] VIEW tên [(cột, ...)] AS truy_vấn` và `DROP VIEW [IF EXISTS] a [, b ...] [CASCADE | RESTRICT]`. Đây là view thường (SQL view). Không liên quan tới "view dạng cột" ở §8 (`internal/sql/views.go`); code của view SQL nằm ở `internal/sql/sqlview.go` và `internal/core/sqlview.go`.

**Lưu trữ.** Mỗi view là một dòng của bảng hệ thống `__stask_views` (cùng cách với sequence, `__stask_sequences`): tên, văn bản SQL của truy vấn, tên các cột kết quả (chốt lúc tạo) và danh sách quan hệ mà truy vấn đọc trực tiếp. Vì là dòng thường nên view đi qua WAL, checkpoint, replay khi khởi động và nhân bản tới replica mà không cần cơ chế riêng. Bảng hệ thống bị ẩn khỏi client, publication và CDC (`Schema.System`): tạo hay xoá view không sinh thay đổi nào trong slot.

**Phân giải.** Khi planner gặp tên không phải bảng, nó tìm view và thay bằng truy vấn con (`expandView` trong `sqlview.go`, gọi từ `compileFrom`). View lồng nhau mở ra đệ quy. Truy vấn của view chạy ở phạm vi riêng: không thấy CTE hay cột của câu dùng nó. Câu `SELECT ... FROM view` một bảng sang bộ thực thi tổng quát (đường nhanh báo lỗi rồi `runHere` thử lại, như với sequence).

**MVCC.** Định nghĩa được đọc bằng `tx.Get` theo snapshot của giao dịch. Giao dịch repeatable read bắt đầu trước `CREATE VIEW` thì không thấy view; bắt đầu trước `CREATE OR REPLACE`/`DROP VIEW` thì vẫn dùng định nghĩa cũ. (Bảng cơ sở thì khác: schema bảng chưa có MVCC, xem §11.)

**Quy tắc.**

- `SELECT *` được chốt thành danh sách cột lúc tạo, như PG: cột thêm vào bảng sau đó không hiện trong view. Tên cột trùng trong view trả `42701`. Tham số `$n` trong truy vấn của view bị từ chối.
- `OR REPLACE`: giữ nguyên tên và kiểu các cột cũ, được thêm cột ở cuối; không thì `42P16`. View chưa có thì tạo mới.
- Tên view cùng không gian tên với bảng và sequence: trùng thì `42P07`. `DROP TABLE` trên view hoặc `DROP VIEW` trên bảng trả `42809`.
- View chỉ đọc: `INSERT/UPDATE/DELETE` qua view trả `55000`.
- **Chặn vòng:** lúc `CREATE`/`OR REPLACE`, truy vấn được biên dịch với chính view đó nằm trên ngăn xếp; chạm lại nó (trực tiếp hay qua view khác) trả `42P17`, không ghi gì.
- **Phụ thuộc:** `DROP VIEW` view còn view khác dùng, hoặc `DROP TABLE` bảng còn view đọc, trả `2BP01` (kèm tên view phụ thuộc) trừ khi `CASCADE`; khi đó các view phụ thuộc, kể cả gián tiếp, bị xoá cùng (một lần ghi nguyên tử, thông báo `drop cascades to view ...`). `DROP VIEW a, b` khi `b` dùng `a` thì được.
- `ALTER TABLE` có `DROP COLUMN`, `ALTER COLUMN TYPE`, `RENAME COLUMN`, `RENAME TO` trên bảng còn view đọc trả `2BP01` (PG cho đổi tên và tự sửa view; ở đây văn bản được lưu nên chưa làm). `ADD COLUMN` thì được.
- **Khác PG:** như `CREATE TABLE`, `CREATE/DROP VIEW` có hiệu lực ngay, kể cả trong `BEGIN`. Phụ thuộc vào sequence (`SELECT last_value FROM seq`) và vào hàm không được theo dõi.

**Chưa làm.** `pg_class`/`pg_views`/`information_schema.views` chưa liệt kê view (để lại `TODO(catalog)` trong `sqlview.go`; `catalog_*.go` thuộc việc khác); `ALTER VIEW`, view tạm, `WITH CHECK OPTION`, `security_barrier`; `\d view` của psql phụ thuộc P4.1.

**Test.** `internal/sql/sqlview_test.go` (SQL, lồng nhau, vòng, `2BP01`, snapshot, khởi động lại bằng replay và bằng checkpoint, CDC, Describe) và `internal/repl/sqlview_repl_test.go` (replica theo CREATE/REPLACE/DROP, khởi động lại replica).

## 17. pg_catalog và information_schema (2026-10-29)

Mã nguồn: `internal/sql/catalog*.go`. Các bảng ảo, dựng từ schema của engine khi câu truy vấn đọc tới, nên câu lệnh thường không tốn gì. Mọi bảng thuộc schema `public` và một role duy nhất (`stask`, OID 10).

**OID.** OID của PostgreSQL cho schema (`pg_catalog` 11, `public` 2200, `information_schema` 13000), kiểu và các bảng catalog. Bảng: 16384 + ID bảng (giữ nguyên qua `TRUNCATE`), index khoá chính: cộng `0x40000000`, sequence: `0x60000000` + hash tên. `'tên'::regclass`, `to_regclass`, `::regtype`, `::regnamespace` và `::oid` hoạt động. Một `regclass` được in ra như số, không như tên.

**Có dữ liệu:** `pg_namespace`, `pg_class` (bảng `r`, index khoá chính `i`, sequence `S`; `relnatts` tính cả cột đã xoá; `reloptions` = `{columnar=true}` và các `WITH (...)` khác), `pg_attribute` (cột đã `DROP COLUMN` hiện như PostgreSQL: `attisdropped`, `atttypid` 0, tên `........pg.dropped.N........`, `attnum` không bị dùng lại; `atthasmissing` khi `ADD COLUMN ... DEFAULT`; `attidentity`), `pg_attrdef`, `pg_type` (các kiểu mà wire trả, cộng mảng), `pg_index` (chỉ khoá chính; `indkey` là `int2vector` in dạng văn bản `1`), `pg_constraint` (`p` và `c`), `pg_sequence`, `pg_depend` (sequence thuộc cột: `a` cho serial, `i` cho identity), `pg_am`, `pg_collation`, `pg_database`, `pg_roles`/`pg_authid`/`pg_user`, `pg_settings`, `pg_tables`, `pg_indexes`, `pg_tablespace`, `pg_publication_rel`; `information_schema.schemata`, `tables`, `columns`, `sequences`, `table_constraints`, `check_constraints`, `key_column_usage`, `constraint_column_usage`, `table_privileges`.

**Rỗng nhưng có đủ cột** (để câu `\d`, JDBC và DBeaver không lỗi): `pg_description`, `pg_inherits`, `pg_trigger`, `pg_proc`, `pg_extension`, `pg_policy`, `pg_statistic_ext`, `pg_publication_namespace`, `pg_rewrite`, `pg_enum`, `pg_range`, `pg_auth_members`, `pg_stat_activity`; `information_schema.referential_constraints`, `views`, `routines`, `triggers`, `domains`, `parameters`.

**Hàm:** `pg_get_expr`, `pg_get_indexdef`, `pg_get_constraintdef`, `pg_get_serial_sequence`, `pg_get_userbyid`, `format_type`, `pg_table_is_visible` (và `_type_`, `_function_`), `current_schema(s)`, `current_database`, `current_user`/`session_user`/`current_catalog` (cả không có dấu ngoặc), `current_setting`, `version`, `obj_description`/`col_description` (NULL), `pg_encoding_to_char`, `quote_ident`, `has_*_privilege` (luôn đúng), `pg_relation_size` (0), `pg_backend_pid`, `pg_is_in_recovery`, `information_schema._pg_expandarray`.

**Cú pháp thêm cho các câu catalog:** `OPERATOR(pg_catalog.~)`, `COLLATE x`, `::pg_catalog.type`, `::"char"`, `x = ANY(mảng)` / `ALL(mảng)`, `ARRAY(SELECT ...)`, `col[i]` (từ 0 cho `int2vector`), toán tử `&`, hàm đặt trong ngoặc kép (`"substring"(...)`), `pg_catalog.generate_series(...) alias(cột)` trong FROM.

**Giới hạn đã biết.** Kiểu hiển thị theo kiểu wire của engine: `integer`/`smallint` hiện `bigint`, `timestamp` hiện `timestamp with time zone`, `varchar(n)` hiện `text` (không có độ dài). `relpages`/`reltuples` là 0/-1. Mọi index là khoá chính, nên `getIndexInfo` trả đúng một dòng mỗi bảng. `regclass` in ra số. Không có view (`relkind` `v`) cho tới khi `CREATE VIEW` có.

**Kiểm chứng.** `internal/sql/catalog_test.go` (từng bảng catalog, `information_schema`, hàm, DDL đổi thì catalog đổi, bảng cột, sequence, `ALTER`/`DROP COLUMN`), `internal/sql/catalog_queries_test.go` (các câu psql 16 `\dt` `\d` `\di` `\ds` `\dn` `\d tbl` `\l` `\dT` `\du`, pgjdbc 42.7 `getTables`/`getColumns`/`getPrimaryKeys`/`getIndexInfo`/`getSchemas`/`getCatalogs`/`getTypeInfo`/`getImportedKeys`, DBeaver, viết lại từ trí nhớ về mã nguồn các client này, chưa đối chiếu với gói tin thật), `internal/pgwire/catalog_wire_test.go` (qua pgx, giao thức mở rộng có tham số và giao thức đơn giản).

## 18. CREATE TRIGGER và DROP TRIGGER (2026-10-29)

Trigger thân SQL theo [SQLite](https://www.sqlite.org/lang_createtrigger.html); [PostgreSQL](https://www.postgresql.org/docs/16/sql-createtrigger.html) cho tên gọi, thứ tự chạy (theo tên) và mã lỗi. Không có ngôn ngữ thủ tục (docs/10 §5).

```sql
CREATE [OR REPLACE] TRIGGER [IF NOT EXISTS] name {BEFORE | AFTER} {INSERT | UPDATE | DELETE} [OR ...]
    ON table [FOR [EACH] ROW] [WHEN (condition)]
BEGIN statement; ... END
DROP TRIGGER [IF EXISTS] name [ON table]
```

Thân là các câu `INSERT`, `UPDATE`, `DELETE`, `SELECT` (kể cả `WITH`); `NEW.col` và `OLD.col` là dòng đang ghi (`OLD` là NULL khi INSERT, `NEW` là NULL khi DELETE; dùng `OLD` trong trigger chỉ INSERT hay `NEW` trong trigger chỉ DELETE là lỗi 42P17 lúc tạo). Hai mở rộng:

- `SET NEW.col = expr [, NEW.col2 = expr2]` trong trigger BEFORE INSERT/UPDATE đổi dòng sắp ghi (SQLite không cho; PostgreSQL làm bằng hàm plpgsql). `RETURNING` thấy giá trị đã đổi. Trigger sau thấy `NEW` đã đổi.
- `SELECT RAISE(ABORT, 'msg') [WHERE cond]` hoặc `SELECT CASE WHEN cond THEN RAISE(ABORT, 'msg') END` làm câu lệnh lỗi `P0001`. `ABORT`, `FAIL`, `ROLLBACK` đều làm cùng việc: câu lệnh lỗi (autocommit thì cả giao dịch bị huỷ; trong `BEGIN` thì giao dịch chuyển sang trạng thái lỗi). `IGNORE` chưa hỗ trợ.

**Chạy.** Mỗi dòng bị INSERT/UPDATE/DELETE chạm tới (kể cả `INSERT ... SELECT`, `UPDATE ... FROM`, `DELETE ... USING`, `ON CONFLICT DO UPDATE`, `COPY FROM`) chạy trigger BEFORE trước khi ghi và trigger AFTER ngay sau khi ghi dòng đó, **trong cùng giao dịch**: thứ trigger ghi thấy được trong giao dịch, commit hay rollback cùng câu lệnh. Đổi khoá chính là một UPDATE. `ON CONFLICT DO NOTHING` bỏ qua dòng thì không chạy AFTER. `ON CONFLICT DO UPDATE` chạy BEFORE INSERT, rồi BEFORE/AFTER UPDATE (như PG). `TRUNCATE` không chạy trigger. Trigger ghi vào bảng có trigger thì chạy tiếp; sâu quá 16 tầng thì lỗi 54001 (`stack depth limit exceeded`) và câu lệnh gốc bị huỷ, nên đệ quy vô hạn dừng và không để lại gì. Không có trigger nào chạy trên replica: replica áp dụng các dòng leader đã ghi (kể cả dòng do trigger ghi), mỗi dòng đúng một lần.

**Lưu trữ.** Như view: một dòng của bảng hệ thống ẩn `__stask_triggers` (khoá `bảng\x1ftên`, cột `tbl, name, timing, events, cond, body`), nên đi qua WAL, checkpoint, replay và nhân bản; ẩn khỏi client, publication và CDC. DDL áp dụng ngay như `CREATE TABLE`. `DROP TABLE` xoá trigger của bảng; `ALTER TABLE ... RENAME TO` bị từ chối (0A000) khi bảng có trigger. Lúc tạo, thân được biên dịch và kiểm tra theo bảng (cột lạ: 42703).

**Giá phải trả.** `Engine.Triggers(bảng)` đọc một con trỏ nguyên tử tới map đã dựng sẵn (bị huỷ mỗi lần tạo/xoá trigger, dựng lại ở lần đọc sau; `gen` chặn lần dựng đua với thay đổi): bảng không có trigger chỉ tốn một lần tra map rỗng mỗi câu lệnh, không thêm cấp phát. Thân được biên dịch một lần cho mỗi định nghĩa (`NEW.col` thành tham số `$n`); mỗi lần chạy tốn chừng 5 µs cho một thân `INSERT` một dòng (`BenchmarkWriteTrigger`).

**Catalog.** `pg_trigger` (tgtype theo bit của PG), `pg_class.relhastriggers`, `pg_tables.hastriggers`. `pg_get_triggerdef` trả NULL, `information_schema.triggers` còn trống.

**Chưa làm.** `UPDATE OF col`, trigger mức câu lệnh, `INSTEAD OF` (cả trên view), `REFERENCING`, `RAISE(IGNORE)`, `CREATE FUNCTION`/`EXECUTE FUNCTION` (P7.2), trigger `DEFERRABLE`, `ALTER TABLE ... DISABLE TRIGGER`.

**Kiểm chứng.** `internal/sql/trigger_test.go` (AFTER và mọi dạng ghi, BEFORE `SET`/`RAISE`, rollback, đệ quy, DDL và lỗi, tham số, khôi phục qua replay và checkpoint, CDC, COPY, catalog), `internal/repl/trigger_repl_test.go` (replica), `BenchmarkWriteNoTrigger`/`BenchmarkWriteTrigger`.
