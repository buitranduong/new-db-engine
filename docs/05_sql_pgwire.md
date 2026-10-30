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

**Kiểu dữ liệu** (P1.1, 2026-10-29). Các tên kiểu PostgreSQL được ánh xạ sang các kiểu lưu trữ sau (định dạng trên đĩa ở docs/09 §4):

| Kiểu SQL | Kiểu lưu | OID trả về |
|---|---|---|
| `int`, `integer`, `bigint`, `smallint`, `serial` | int64 | int8 (20) |
| `real`, `float`, `double precision` | float64 | float8 (701) |
| `text`, `varchar(n)`, `char(n)` | chuỗi | text (25) |
| `boolean` | bool | bool (16) |
| `timestamp [without time zone]` | micro giây từ 1970-01-01 | timestamp (1114) |
| `timestamptz`, `timestamp with time zone` | micro giây từ 1970-01-01, UTC | timestamptz (1184) |
| `date` | ngày (chỉ trong biểu thức) | date (1082) |
| `numeric`, `numeric(p,s)`, `decimal` | số thập phân độ chính xác tuỳ ý (như `numeric` của PG) | numeric (1700) |
| `uuid` | 16 byte | uuid (2950) |
| `bytea` | byte | bytea (17) |
| `json` | văn bản đã kiểm tra cú pháp, giữ nguyên | json (114) |
| `jsonb` | văn bản chuẩn hoá như PG in (khoá xếp theo độ dài rồi byte, bỏ khoá trùng, `": "` và `", "`) | jsonb (3802) |
| `int[]`, `text[]`, `numeric[]`, `uuid[]`, `timestamptz[]`, `double precision[]`, `boolean[]` (một chiều) | văn bản dạng mảng của PG, phần tử được kiểm tra theo kiểu | bigint[] (1016), text[] (1009), numeric[] (1231), uuid[] (2951), timestamptz[] (1185), float8[] (1022), bool[] (1000) khi qua CDC; kết quả truy vấn vẫn khai là text[] (1009) |

- **Số học `numeric`** theo PG: cộng/trừ lấy scale lớn hơn, nhân cộng scale, chia theo `select_div_scale` (ít nhất 16 chữ số có nghĩa), `round` làm tròn ra xa số 0, `numeric(p,s)` làm tròn về scale `s` và báo `22003` khi tràn. `NaN`, `Infinity` có. Literal thập phân (`1.5`, `1e3`) là `numeric`, như PG. `numeric` với `double precision` cho `double precision`. `sum`/`avg`/`min`/`max` trên `numeric` chính xác. `EXTRACT` trả `numeric` (scale 6 cho giây và epoch, 3 cho mili giây).
- **Timestamp** giữ đủ micro giây; đầu vào làm tròn về micro giây. Cộng `interval` theo lịch (31/1 + 1 tháng = 29/2). Số nguyên gán cho cột timestamp được hiểu là mili giây (để giữ cách dùng cũ).
- `json`/`jsonb`/`uuid`/`bytea` sai cú pháp trả `22P02`. Chưa có toán tử `->`, `->>`, `@>` và hàm JSON.
- **Chưa làm:** mảng nhiều chiều; `int2`/`int4`/`varchar(n)` vẫn hiện là `bigint`/`text`; `time`, `interval` làm kiểu cột; `numeric`, `uuid`... trên bảng `columnar` (`numeric` bị từ chối khi tạo bảng columnar, các kiểu khác dùng được); khoá chính chỉ là `int`, `string` hoặc `uuid`.
- **Bảng cũ:** cột `time` (mili giây, tạo trước P1.1) vẫn đọc và ghi được như trước (độ phân giải mili giây) và hiện là `timestamptz`. `ALTER TABLE t ALTER COLUMN c TYPE timestamptz` chuyển nó sang micro giây mà không viết lại dòng nào (docs/09 §5); cũng chuyển được `int`/`float` sang `numeric`.

**Câu lệnh:**
- `CREATE TABLE [IF NOT EXISTS]` với khoá chính một cột, **nhiều cột** hoặc **không có khoá chính** (§24), `NOT NULL`, và `WITH (...)` để bật các view dẫn xuất: `columnar`, `search='cột1,cột2'`, `append_only`, `time_column`, `series_column`.
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

- `CREATE VIEW`, `ARRAY JOIN` của ClickHouse, `FINAL`/`SAMPLE` (được chấp nhận nhưng bỏ qua).
- Truy vấn nhiều bảng chạy bằng bộ thực thi tổng quát trên dữ liệu đã đưa hết vào bộ nhớ (hash join, không spill ra đĩa). Chỉ aggregate một bảng mới dùng view dạng cột.
- Index phụ. Mọi truy vấn không theo khoá chính đều quét bảng.
- `ALTER COLUMN ... TYPE` giữa kiểu số/chuỗi/bool/thời gian đã có (docs/14 K4); khoá ngoại, `UNIQUE` ngoài khoá chính. `ALTER TABLE` đã có, xem §11. `COPY` text/csv đã có, xem §14 (chưa có binary, file/program, `FORCE_*`, giao thức mở rộng).
- `pg_catalog` và `information_schema`. Lệnh `\d` của psql, cùng phần duyệt schema của DBeaver và DataGrip, sẽ báo lỗi. Truy vấn dữ liệu thì vẫn chạy.
- Xác thực bằng chứng chỉ client, GSSAPI (xem §10). Vai trò và `GRANT`/`REVOKE` mức bảng đã có từ 2026-10-29, xem §18; quan sát vận hành (`pg_stat_activity`, `/metrics`, log chậm, scrub) xem §19. (Huỷ câu lệnh và các timeout của session đã có từ 2026-10-29, xem §11.)
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
- Mật khẩu chung `-pg-password` vẫn là đường đăng nhập của superuser khởi tạo; user riêng có role và mật khẩu riêng, xem §18. Không có `pg_hba.conf` theo host/user/database.
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
| `ALTER COLUMN TYPE` | Đã làm cho kiểu cơ bản (docs/14 K4): không viết lại dòng, dùng cơ chế bố cục theo LSN, `USING col::type` cho phép ép kiểu tường minh | Thêm các cặp của P1.1: `time`→`timestamp`/`timestamptz`, `int`/`float`→`numeric` (docs/09 §4); chúng không cần `USING` và không kiểm lại từng dòng.
| DROP COLUMN trên bảng time-series (`0A000`), hoặc chính cột đang được view search đánh chỉ mục | Hoãn: view time-series lưu block theo vị trí cột. Bảng time-series là append-only nên hiếm khi bị đổi. Các cột khác của bảng có search thì xoá được |
| DROP COLUMN cột thuộc khoá chính (`0A000`) | Không làm, vì bảng STASK luôn cần khoá nội bộ. PG cho phép và bỏ luôn ràng buộc |
| Cột thêm bằng ADD COLUMN không được lưu trong view cột cho tới lần khởi động sau (aggregate trên cột đó chạy trên dòng) | Hoãn: dựng vector ở nền |
| SET NOT NULL và ADD CHECK kiểm tra trên snapshot. Một dòng vi phạm được ghi đúng lúc ALTER đang kiểm tra có thể lọt | Hoãn tới khi có khoá bảng (P5.2). PG dùng `ACCESS EXCLUSIVE` |
| `ALTER TABLE ... ADD PRIMARY KEY` chỉ trên bảng **rỗng** (§24) | Thêm khoá chính cho bảng có dòng phải gắn lại khoá của mọi dòng (viết lại bảng): chưa làm, báo `0A000`. UNIQUE và FOREIGN KEY đã có (§21) |

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

**Giới hạn đã biết.** Kiểu hiển thị theo kiểu của engine: `integer`/`smallint` hiện `bigint`, `varchar(n)` hiện `text` (không có độ dài); `timestamp`, `timestamptz`, `numeric(p,s)` (qua `atttypmod`), `uuid`, `bytea`, `json`, `jsonb` và các mảng hiện đúng kiểu từ P1.1. `relpages`/`reltuples` là 0/-1. Index khoá chính chỉ có khi bảng có khoá chính (không có dòng nào cho bảng không PK); khoá ghép hiện đúng `indkey = "1 2"`, `conkey = {1,2}`. Cột ẩn `__rowid`/`__pk` (§24) không có trong `pg_attribute`/`information_schema`, và `attnum` của cột thật vẫn bắt đầu từ 1. `regclass` in ra số. Không có view (`relkind` `v`) cho tới khi `CREATE VIEW` có.

**Kiểm chứng.** `internal/sql/catalog_test.go` (từng bảng catalog, `information_schema`, hàm, DDL đổi thì catalog đổi, bảng cột, sequence, `ALTER`/`DROP COLUMN`), `internal/sql/catalog_queries_test.go` (các câu psql 16 `\dt` `\d` `\di` `\ds` `\dn` `\d tbl` `\l` `\dT` `\du`, pgjdbc 42.7 `getTables`/`getColumns`/`getPrimaryKeys`/`getIndexInfo`/`getSchemas`/`getCatalogs`/`getTypeInfo`/`getImportedKeys`, DBeaver, viết lại từ trí nhớ về mã nguồn các client này, chưa đối chiếu với gói tin thật), `internal/pgwire/catalog_wire_test.go` (qua pgx, giao thức mở rộng có tham số và giao thức đơn giản).

## 18. Vai trò, GRANT/REVOKE và quyền mức bảng (2026-10-29)

Hạng mục P5.2 (và S4) của lộ trình. Mã nguồn: `internal/core/roles.go` (lưu trữ), `internal/core/scram.go` (verifier), `internal/sql/roles.go` (cú pháp, kiểm quyền), `internal/pgwire/auth.go` (đăng nhập). Test: `internal/sql/roles_test.go`, `internal/pgwire/roles_test.go`.

**Cách các hệ khác làm** ([CREATE ROLE](https://www.postgresql.org/docs/16/sql-createrole.html), [GRANT](https://www.postgresql.org/docs/16/sql-grant.html), [pg_authid](https://www.postgresql.org/docs/16/catalog-pg-authid.html), [thuộc tính REPLICATION](https://www.postgresql.org/docs/16/role-attributes.html), CockroachDB [`GRANT`](https://www.cockroachlabs.com/docs/stable/grant)): verifier SCRAM lưu trong catalog, quyền là bitmask theo bảng, walsender cần thuộc tính `REPLICATION`. STASK làm đúng cú pháp và mã lỗi đó, ở mức tối thiểu.

### Cú pháp

| Lệnh | Ghi chú |
|---|---|
| `CREATE ROLE tên [WITH] opt...`, `CREATE USER ...` | `CREATE USER` mặc định `LOGIN`, `CREATE ROLE` mặc định `NOLOGIN`. Opt: `SUPERUSER`/`NOSUPERUSER`, `LOGIN`/`NOLOGIN`, `REPLICATION`/`NOREPLICATION`, `[ENCRYPTED] PASSWORD 'x'`, `PASSWORD NULL`. `NOCREATEDB`, `NOCREATEROLE`, `INHERIT`, `NOINHERIT`, `NOBYPASSRLS` được nhận (là mặc định). `CREATEDB`, `CREATEROLE`, `CONNECTION LIMIT`, `VALID UNTIL`, `IN ROLE`, `ADMIN` bị từ chối `0A000`. |
| `ALTER ROLE\|USER tên [WITH] opt...` | Cùng opt. Không có `RENAME`, `SET`. Tự bỏ `SUPERUSER` của chính mình bị từ chối `42501`. |
| `DROP ROLE\|USER [IF EXISTS] a, b` | `2BP01` nếu role còn quyền trên bảng (phải `REVOKE` trước, như PG); `55006` nếu là role của chính session. |
| `GRANT p[, p] ON [TABLE] t[, ...] TO r[, ...]` | `p` = `SELECT`, `INSERT`, `UPDATE`, `DELETE`, `ALL [PRIVILEGES]`. `r` là role hoặc `PUBLIC`. Cũng nhận `ON ALL TABLES IN SCHEMA public` (các bảng có lúc chạy lệnh; chưa có `ALTER DEFAULT PRIVILEGES`). Đối tượng là view cũng được. |
| `REVOKE p ON t FROM r [CASCADE\|RESTRICT]` | Như trên. |
| `GRANT USAGE ON SCHEMA public TO r`, `GRANT CONNECT ON DATABASE d TO r` | Không làm gì (luôn được phép); nhận để các công cụ chạy được. |

Bị từ chối `0A000`: `WITH GRANT OPTION`, quyền theo cột, `TRUNCATE`/`REFERENCES`/`TRIGGER`, quyền trên sequence và hàm, thành viên role (`GRANT role TO role`), `CREATE` trên schema.

### Lưu trữ

Mỗi role là một dòng của bảng hệ thống ẩn `__stask_roles` (cùng mẫu với `__stask_views`, `core/sqlview.go`): đi qua WAL, checkpoint, replay, nhân bản; ẩn khỏi client, publication và CDC. Cột: `name` (khoá), `flags` (bit 0 superuser, 1 login, 2 replication), `verifier` (`SCRAM-SHA-256$4096:<salt>$<StoredKey>:<ServerKey>`, đúng dạng `pg_authid.rolpassword`, mật khẩu không khôi phục được), `grants` (bản ghi `bits\x01bảng` cách nhau bằng NUL, bits: 1 SELECT, 2 INSERT, 4 UPDATE, 8 DELETE). Dòng tên `public` giữ quyền của `PUBLIC`; không phải role. Ghi role/quyền là đọc-sửa-ghi dưới một mutex; `GRANT` nhiều role là một ghi nguyên tử. `DROP TABLE` xoá quyền của bảng đó, `ALTER TABLE RENAME` chuyển quyền theo tên mới (không để quyền sống lâu hơn bảng).

`DROP ROLE` chỉ xoá khi hết quyền, nên không có "đối tượng sở hữu" nào cần chuyển: **STASK không có chủ sở hữu bảng**. Hệ quả: DDL (`CREATE/ALTER/DROP TABLE`, `TRUNCATE`, view, sequence, publication, role, `GRANT`) chỉ dành cho superuser. Muốn một role không phải superuser tạo bảng thì chưa được.

### Đăng nhập

- Tên user trong startup packet trùng một role đã lưu: role phải `LOGIN`, không thì FATAL `28000 role "x" is not permitted to log in`. Role có mật khẩu luôn phải chứng minh: **SCRAM-SHA-256** (và `-PLUS` qua TLS) so với verifier đã lưu, hoặc cleartext nếu server chạy `-pg-auth password` (server tính lại verifier từ mật khẩu gửi tới rồi so sánh constant-time). Sai: `28P01`. Role không có mật khẩu chỉ vào được khi server chạy `trust`.
- Tên user không phải role: như trước, theo `-pg-auth`/`-pg-password` chung, session là **superuser khởi tạo (bootstrap)** không bị kiểm quyền. Mọi triển khai chưa dùng role chạy y như cũ.
- `-pg-roles-only`: khi đã có một role `SUPERUSER LOGIN`, đóng đường đăng nhập bằng mật khẩu chung (user lạ trải qua cùng chuỗi SCRAM giả rồi nhận `28P01`, để không lộ user nào có thật). Chưa có role superuser thì mật khẩu chung vẫn dùng được, tránh tự khoá mình.
- Đổi mật khẩu hay `NOLOGIN` có hiệu lực ở lần đăng nhập sau; session đang mở giữ nguyên cho tới khi đóng.
- Đếm lần đăng nhập thất bại ở `stask_pg_auth_failures_total` (§19). Chưa có trễ tăng dần/khoá tạm (S5) và chưa có audit (S7).

### Kiểm quyền

Mỗi câu lệnh của session có role được kiểm trước khi chạy (`Session.authorize`, một dòng ở `exec`, thêm ở `Describe` và `COPY`): lỗi `42501 permission denied for table t`, trong BEGIN thì giao dịch vào trạng thái lỗi như mọi lỗi khác. Quyền được đọc từ dòng role mới nhất ở mỗi câu lệnh, nên `GRANT`/`REVOKE`/`DROP ROLE` có hiệu lực ngay với session đang mở. Danh sách bảng và quyền cần của một câu lấy từ cây cú pháp (duyệt bằng reflection, một lần cho mỗi prepared statement; không phải tự liệt kê từng loại biểu thức nên subquery, CTE, UNION, JOIN, `INSERT ... SELECT` đều được phủ):

| Câu | Cần |
|---|---|
| `SELECT` (mọi bảng/view trong FROM, kể cả trong subquery, CTE, UNION) | `SELECT` trên từng bảng. Tên CTE che bảng cùng tên chỉ trong phạm vi định nghĩa nó (`WITH t AS (SELECT * FROM t)` vẫn kiểm bảng `t`). |
| `INSERT` | `INSERT`; thêm `SELECT` nếu có `RETURNING` hoặc `ON CONFLICT`, thêm `UPDATE` nếu `DO UPDATE`; phần `SELECT` nguồn kiểm riêng. |
| `UPDATE` / `DELETE` | `UPDATE` / `DELETE`; thêm `SELECT` khi có `WHERE`, `FROM`/`USING` hoặc `RETURNING` (như PG). |
| `COPY t FROM` / `COPY t TO` | `INSERT` / `SELECT`. `COPY (query) TO` kiểm như query. |
| `BEGIN`, `COMMIT`, `ROLLBACK`, `SET`, `SHOW`, `LISTEN`, `NOTIFY` | không cần |
| catalog (`pg_*`, `information_schema`), hàm bảng, sequence, `nextval` | không kiểm (đọc catalog luôn được; **sequence chưa có quyền riêng**) |
| mọi DDL, `GRANT`, role | superuser |

View được kiểm như một quan hệ riêng: quyền `SELECT` trên view đủ, không cần quyền trên bảng bên dưới (chủ view luôn là superuser). `has_table_privilege([role,] bảng, quyền)` trả theo quyền thật; `information_schema.table_privileges` liệt kê thêm các quyền đã `GRANT`; `pg_roles`, `pg_authid`, `pg_user` liệt kê role (mật khẩu hiện `********`); `current_user` trả role của session.

### Nhân bản và CDC

- **Walsender** (`replication=database`): khi role đã lưu không phải superuser và không có `REPLICATION`, FATAL `42501 permission denied to start WAL sender` ngay lúc khởi động (PG cũng làm vậy). Role `REPLICATION` mở được walsender, tạo slot và stream publication **không cần quyền `SELECT` trên bảng** (đúng PG: quyền stream là thuộc tính `REPLICATION`; quyền `SELECT` chỉ cần cho đồng bộ ban đầu bằng `COPY`/`SELECT`). Câu SQL thường gửi trên kết nối replication vẫn bị kiểm quyền bảng. Test `TestReplicationNeedsPrivilege`: `plain` bị `42501`, `repl` stream được dòng `INSERT` qua `START_REPLICATION`, đồng thời không `SELECT` được bảng.
- **Nhân bản nhị phân `-repl-addr`** (leader/replica) có xác thực riêng bằng secret/mTLS (S1), không đi qua role. Replica nhận các dòng `__stask_roles` như mọi dòng: session có role trên replica bị kiểm quyền theo leader, `REVOKE` ở leader có hiệu lực ngay trên replica đang chạy, và role còn nguyên sau khi replica khởi động lại (`TestReplicaRoles`, `internal/repl/roles_repl_test.go`).
- **CDC qua HTTP** (`/v1/cdc`) vẫn dùng token HTTP (docs/13), chưa gắn với role.

### Giới hạn

Không có chủ sở hữu đối tượng, thành viên role (`GRANT role TO role`), quyền cột, quyền sequence/hàm/schema, `WITH GRANT OPTION`, `ALTER DEFAULT PRIVILEGES`, row-level security, `pg_hba.conf` theo IP/database. Hàm nhận tên bảng dạng chuỗi (ví dụ truy vấn catalog) không kiểm quyền. API HTTP và `/metrics` không dùng role. SASLprep chưa có (docs/05 §10). Một superuser mất quyền vẫn có thể tự khôi phục nhờ đăng nhập bằng mật khẩu chung (nếu chưa `-pg-roles-only`).

## 19. Quan sát vận hành: pg_stat_activity, /metrics, log truy vấn chậm, scrub (2026-10-29)

Hạng mục P5.5 và D2. Mã nguồn: `internal/sql/activity.go`, `internal/server/metrics.go`, `internal/core/scrub.go`. Test: `internal/sql/roles_test.go` (`TestActivityAndSlowLog`), `internal/server/metrics_test.go`, `internal/core/scrub_test.go`.

**Cách các hệ khác làm.** PG: [`pg_stat_activity`](https://www.postgresql.org/docs/16/monitoring-stats.html#MONITORING-PG-STAT-ACTIVITY-VIEW) (mỗi backend một dòng, `query` của người khác ẩn với user không phải superuser), [`log_min_duration_statement`](https://www.postgresql.org/docs/16/runtime-config-logging.html), exporter [`postgres_exporter`](https://github.com/prometheus-community/postgres_exporter); CockroachDB phục vụ `/_status/vars` ở định dạng Prometheus ([monitoring](https://www.cockroachlabs.com/docs/stable/monitoring-and-alerting)); định dạng [Prometheus text](https://prometheus.io/docs/instrumenting/exposition_formats/). Quét CRC nền: [`pg_checksums`/`data_checksums`](https://www.postgresql.org/docs/16/app-pgchecksums.html) và [`amcheck`](https://www.postgresql.org/docs/16/amcheck.html) của PG, `scrub` của ZFS/Btrfs.

### pg_stat_activity

Mỗi session PG-wire đã xác thực có một bản ghi (`Session.Track`, gỡ khi đóng). Cột: `datid, datname, pid, leader_pid, usesysid, usename, application_name, client_addr, client_hostname, client_port, backend_start, xact_start, query_start, state_change, wait_event_type, wait_event, state, backend_xid, backend_xmin, query_id, query, backend_type` (cột tối thiểu theo yêu cầu: `pid, usename, state, query, query_start, xact_start`; các cột khác để công cụ không lỗi, một số luôn NULL). `state`: `active`, `idle`, `idle in transaction`, `idle in transaction (aborted)`. `query` là câu đang chạy hoặc câu cuối cùng (như PG); user không phải superuser thấy `<insufficient privilege>` ở dòng của người khác. Câu `CREATE/ALTER ROLE` được thay bằng `CREATE ROLE <password hidden>` để mật khẩu không nằm ở đây hay trong log chậm. Chỉ có session PG-wire; phiên HTTP không có. Chưa có `pg_cancel_backend`/`pg_terminate_backend`.

### /metrics

`GET /metrics` cạnh API HTTP, cùng token (token chỉ đọc đủ; không có token → 401 khi API có xác thực). Định dạng Prometheus text 0.0.4. Nguồn: các bộ đếm atomic đã có của engine và của lớp SQL; không tính toán theo lần scrape, trừ `runtime.ReadMemStats`.

| Metric | Loại |
|---|---|
| `stask_up`, `stask_uptime_seconds`, `stask_durable_lsn` | gauge/counter |
| `stask_commits_total`, `stask_conflicts_total`, `stask_commit_batches_total`, `stask_fsyncs_total`, `stask_fsync_seconds_total`, `stask_fsync_slow_total` | counter |
| `stask_wal_written_bytes_total`, `stask_wal_segments`, `stask_wal_disk_bytes`, `stask_checkpoints_total`, `stask_checkpoint_bytes_total` | counter/gauge |
| `stask_gc_rounds_total`, `stask_gc_reclaimed_segments_total`, `stask_gc_pinned_snapshots`, `stask_gc_oldest_snapshot_age_seconds` | counter/gauge |
| `stask_tables`, `stask_live_rows`, `stask_http_open_transactions`, `stask_go_heap_alloc_bytes`, `stask_go_goroutines` | gauge |
| `stask_sql_statements_total{kind}` (select, insert, update, delete, transaction, ddl, other), `stask_sql_statement_errors_total`, `stask_sql_permission_denied_total`, `stask_sql_slow_statements_total` | counter |
| `stask_sql_statement_duration_seconds` (histogram, 100 µs … 30 s) | histogram |
| `stask_pg_sessions{state}`, `stask_pg_sessions_opened_total`, `stask_pg_auth_failures_total` | gauge/counter |
| `stask_scrub_running`, `stask_scrub_errors_total`, `stask_scrub_last_start_timestamp_seconds`, `stask_scrub_last_ok` | gauge/counter |

Thời gian câu lệnh là thời gian thực thi trong engine, **không gồm** lúc chờ fsync của commit autocommit (xem comment ở `Session.run`); đo ở phía client sẽ lớn hơn. Bộ đếm theo tiến trình, không theo engine. Cảnh báo gợi ý: `stask_up == 0`, `increase(stask_scrub_errors_total[1h]) > 0`, `stask_pg_sessions{state="idle_in_transaction"}` lớn, `rate(stask_sql_permission_denied_total[5m])` tăng đột ngột, `histogram_quantile(0.99, ...)` vượt ngưỡng.

### Log truy vấn chậm

`-slow-query 250ms` (hoặc `0` = tắt): câu nào chạy từ ngưỡng đó trở lên được ghi bằng log của staskd: `slow statement: duration: 312.481 ms  pid=17 user="app" statement: SELECT ...` (câu cắt ở 4096 byte). Dùng `Session.SetQuery` do pgwire gọi nên có cả đường extended protocol; câu role được che mật khẩu. Ngưỡng thấp hơn thời gian chạy tối thiểu thì ghi mọi câu: đừng đặt `1ns` ngoài test.

### Chi phí

Mỗi câu lệnh: hai lần đọc đồng hồ, khoảng 10 phép cộng atomic trên bộ đếm **của riêng session** (không tranh chấp giữa core; bộ đếm chung chỉ nhận phần của session đã đóng), một lần khoá mutex rẻ để thay `query`, vài phép gán atomic cho trạng thái. Đo bằng `BenchmarkWirePoint` (`internal/pgwire/wbench_test.go`: pgx, 4 kết nối, `SELECT` theo khoá qua loopback, ghim `taskset -c 4-7`, đo CPU `Getrusage` của cả server lẫn client trong một tiến trình, 5 lượt xen kẽ trước/sau): **45.597 ns CPU/lệnh trước, 46.147 ns sau (+1,2%, tính cả CPU client; nhiễu giữa các lượt ±1,3%)**. Phần của server vào khoảng một nửa tổng, nên đóng góp thực ước tính 1–2% CPU của server ở tải toàn point-select qua loopback, và dưới 1% với tải có đĩa/mạng thật. Chưa chứng minh được "≤ 1%" chắc chắn bằng đo này (máy dùng chung, nhiễu cùng cỡ). Kiểm quyền cho session có role: +1,8 µs/lệnh (3.8 → 5.7 µs trong `BenchmarkPointSelect`), session bootstrap không tốn thêm.

### Scrub (D2)

`POST /v1/admin/scrub` (token đầy đủ) bắt đầu quét nền; `?rate_mb=N` (mặc định 64 MB/s, `0` = không giới hạn), `?wait=1` đợi xong và trả kết quả (HTTP 500 nếu thấy hỏng), `?cancel=1` dừng. `GET /v1/admin/scrub` (token chỉ đọc đủ) trả trạng thái: `running, started, finished, segments, records, checkpoints, bytes, skipped, errors[], cancelled`. `-scrub-interval 24h -scrub-rate-mb 32` chạy định kỳ. Quét:
- Mỗi segment log (kể cả `.walz`): đọc từng bản ghi qua `wal.Scan`, kiểm CRC32C và khung. Segment đang ghi chỉ quét tới vị trí ghi hiện tại. Segment mà GC thu hồi giữa chừng được tính vào `skipped`.
- Segment đã đóng: phần sau bản ghi cuối phải toàn số 0. Nếu không, một trường độ dài bị hỏng thành 0 sẽ giống dấu hết log và che mọi bản ghi sau nó (`TestScrubTailGarbage`).
- Mọi file checkpoint: header và CRC từng chunk (`ReadCheckpoint`; đọc cả file vào bộ nhớ, nên trên checkpoint rất lớn cần chừa RAM tương đương).
- Lỗi: một dòng cho mỗi hỏng hóc trong `errors` (`segment 3: bad record (CRC or framing) at offset 4096, after 17 good records`), ghi log, cộng `stask_scrub_errors_total`. Scrub **chỉ đọc**, không sửa: phục hồi bằng replica hoặc backup (§14).
- Một lần một scrub (`409` nếu đang chạy). Đọc bị giới hạn tốc độ nhưng vẫn đi qua page cache, nên có thể đẩy dữ liệu nóng ra; trên máy sát RAM hãy đặt `rate_mb` thấp. Giá trị trong `Value` của bản ghi không có CRC riêng ngoài CRC của khung, nên được phủ như phần còn lại của bản ghi; index trong bộ nhớ không được quét.

### Kiểm chứng

- `TestRolesAndGrants`, `TestRolesSurviveRestart` (replay và checkpoint), `TestActivityAndSlowLog` (`internal/sql`); `TestReplicaRoles` (`internal/repl`).
- `TestRolesOverWire` (SCRAM và cleartext: `28P01`/`28000`/`42501`, đổi mật khẩu, `COPY`, `pg_stat_activity` qua pgx), `TestRolesOnlyClosesSharedPassword`, `TestReplicationNeedsPrivilege` (walsender và luồng logical thật) (`internal/pgwire`).
- `TestMetricsAndScrubEndpoints` (`internal/server`: 401 khi thiếu token, định dạng mẫu, token chỉ đọc không bắt đầu được scrub), `TestScrub`, `TestScrubTailGarbage`, `TestScrubCompressedSegment` (`internal/core`: lật một byte trong segment và trong checkpoint, đúng một lỗi được báo; khôi phục byte thì sạch; huỷ giữa chừng).

## 20. CREATE TRIGGER và DROP TRIGGER (2026-10-29)

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
## 21. Index phụ, UNIQUE và khoá ngoại (2026-10-29)

Mã nguồn: `internal/core/secindex.go`, `internal/core/fk.go`, `internal/schema/indexkey.go`, `internal/sql/indexddl.go`, `indexpath.go`, `parse_index.go`, `catalog_idx.go`.

**Cú pháp.** `CREATE [UNIQUE] INDEX [CONCURRENTLY] [IF NOT EXISTS] [tên] ON bảng [USING btree] (cột [ASC|DESC] [NULLS ...], ...)`, `DROP INDEX [IF EXISTS] tên, ...`; ràng buộc `UNIQUE` (cột hoặc bảng, có `CONSTRAINT tên`), `REFERENCES bảng [(cột)] [ON DELETE NO ACTION|RESTRICT|CASCADE|SET NULL]`, `FOREIGN KEY (cột) REFERENCES ...` trong `CREATE TABLE`; `ALTER TABLE ... ADD [CONSTRAINT tên] UNIQUE (...)`, `ADD [CONSTRAINT tên] FOREIGN KEY ...`, `DROP CONSTRAINT`. Kiểu cột index được: `bigint`, `double`, `text`/`string`, `timestamp`, `boolean`. **Index trên biểu thức** (nhánh `exprindex`): mỗi phần tử index có thể là hàm bất biến của **một cột**: `lower`, `upper`, `trim`/`btrim`, `ltrim`, `rtrim`, `length`/`char_length` (cột text/string), `abs` (bigint/double), kể cả `UNIQUE` và trộn với cột thường (`(abs(n), upper(tag))`); `WHERE lower(email) = ...` (`=`, `IN`, khoảng) dùng index, `EXPLAIN` hiện `Index Cond: lower(email) = ...`; `pg_indexes`/`pg_get_indexdef` hiện `lower(email)`. Biểu thức khác (`a || b`, `md5(x)`, `lower(a||b)`) vẫn `0A000`. Chỉ lưu tên hàm (`exprs` trong định nghĩa index của checkpoint, `omitempty`), khoá index dựng lại từ dữ liệu khi phục hồi. Index biểu thức không dùng làm đích khoá ngoại. Từ chối bằng `0A000` (không lờ đi): index từng phần ngoài dạng đóng của `schema/indexpred.go` (từ nhánh `pgcmds` đã có `WHERE col IS NULL`, `col <op> literal`; `INCLUDE`, `WITH (..)`, `USING hash/brin` được nhận và chạy như btree: docs/14 Phụ lục P), trên biểu thức ngoài tập trên, phương thức khác btree, `ON UPDATE CASCADE/SET NULL`, `SET DEFAULT`, `MATCH FULL`, ràng buộc `DEFERRABLE`, `ADD COLUMN ... UNIQUE/REFERENCES` (thêm cột rồi thêm ràng buộc bằng câu riêng). `ASC/DESC/NULLS` được nhận và bỏ qua: một index đọc xuôi hoặc ngược.

**Cấu trúc.** Một index là tập entry có thứ tự `Enc(cột1) ‖ Enc(cột2) ‖ ... ‖ khoá chính`, đặt trong cùng loại B+tree copy-on-write của index chính (`index.go`), không có giá trị: khoá chính là con trỏ. Nhờ vậy GC dời một dòng trong log không phải sửa index. Mã hoá khoá nằm trọn trong `schema/indexkey.go` (`AppendKeyValue`, `KeyValueLen`, `IndexableType`): mỗi giá trị mã hoá tự phân định (prefix-free) và so byte đúng thứ tự giá trị; NULL xếp sau cùng (như `ASC` của PG, quét ngược cho `NULLS FIRST` của `DESC`). Số nguyên, thời gian, bool: 8 byte big-endian đảo bit dấu; float: đảo bit theo dấu, `-0` = `0`, NaN trên `+Inf`; chuỗi: byte thô, `0x00` thành `0x00 0xFF`, kết thúc `0x00 0x01`. Khi đổi kiểu timestamp/numeric/uuid chỉ cần thêm một `case` vào hai hàm đó.

**MVCC.** Giống btree của PG, index không biết version nào của dòng có giá trị đó. Entry chỉ được **thêm** khi một version có giá trị đó được ghi. Quét index đọc dòng nhìn thấy được bởi snapshot qua index chính rồi **kiểm lại** rằng cột index của dòng vẫn bằng entry (`recheck`): entry cũ bị bỏ qua, và mỗi dòng chỉ ra một lần vì entry là duy nhất theo (giá trị, khoá chính). Snapshot cũ hơn lúc index dựng xong (`minSnap`) không dùng index (planner quay về quét bảng).

**Duy trì.** Committer thêm entry của cả batch ngay khi chuẩn bị (`prepare`), nên trước khi commit được báo bền thì entry đã có trong cây, không cần nhìn overlay. Thêm entry thừa là vô hại vì có recheck. Installer đánh dấu entry của version bị thay là "chết tại LSN L" khi giá trị index đổi (phải đọc dòng cũ; chi phí này nằm trong số đo bên dưới). Khi horizon GC qua L, `sweepIndexes` (gọi trong chu kỳ GC) xoá entry. Entry được thêm lại sau khi chết (giá trị quay về) sống lại: so LSN thêm với LSN chết nên không đánh dấu nhầm.

**Dựng.** `CREATE INDEX` chặn người ghi vào bảng đó (đếm người ghi trong `submit`, kiểu Dekker), đợi commit đang bay được cài, dựng cây từ đầu các khoá (sắp xếp rồi nạp theo lô), kiểm tra UNIQUE, gắn index rồi ghi bản ghi DDL (`ALTER`) kèm `indexes` trong schema. PG cũng chặn ghi khi `CREATE INDEX` thường; `CONCURRENTLY` được nhận nhưng chạy như thường. Bảng khác và người đọc không bị chặn. Giao dịch đã mã hoá dòng trước DDL gặp `ErrConflict` khi commit (cơ chế ALTER có sẵn), autocommit tự làm lại.

**Checkpoint, khởi động, nhân bản.** Checkpoint **không** chứa entry, chỉ định nghĩa (trong `tables[].indexes`). Khởi động: nạp checkpoint, dựng lại mọi index từ index chính (`rebuildSec`, đọc từng dòng đầu), rồi replay log duy trì tiếp. DDL thêm/xoá index khi replay thì dựng/xoá theo đó. Replica đi cùng đường: bản ghi DDL tới thì dựng từ các dòng đã áp dụng, mỗi bản ghi ghi tới thì thêm entry và đánh dấu chết (`secReplay`). **Chi phí khởi động** tỉ lệ số dòng × số index (đọc mọi dòng đầu); chưa đo ở 100M dòng. Lưu entry vào checkpoint là việc sau nếu số này quá lớn.

**UNIQUE.** Quyết định trong committer (một goroutine, thấy mọi commit theo thứ tự), nên hai giao dịch cùng chèn một giá trị thì đúng một bên thắng, bên kia nhận `23505` (test 8 goroutine × 50 vòng). Committer duyệt entry cùng giá trị, bỏ entry chết, và với mỗi ứng viên hỏi giá trị mới nhất của dòng đó từ: các op của chính request, batch đang chuẩn bị, các batch đang bay (bảng `sk` giữ khoá index của đầu mới nhất), rồi index chính. NULL không bao giờ xung đột. Trong `BEGIN ... COMMIT`, `Tx.add` dò trước (cùng quy tắc, cộng các ghi của chính giao dịch) để lỗi xuất hiện ở câu lệnh gây ra chứ không phải ở `COMMIT`; câu autocommit bỏ bước dò, lỗi tới ở commit của chính nó. Thông báo: `duplicate key value violates unique constraint "tên": Key (cột)=(giá trị) already exists.`

**Khoá ngoại.** `REFERENCES` giải thành cột cha là khoá chính hoặc một UNIQUE trùng đúng danh sách cột (kiểu phải cùng loại). Mỗi khoá ngoại tạo một index "nội bộ" trên các cột con (ẩn khỏi catalog, đi cùng ràng buộc) để xoá/sửa cha tìm con nhanh. Kiểm tra lúc câu lệnh, theo snapshot cộng các ghi của giao dịch: chèn/sửa con phải có cha (cột con NULL thì thoả, `MATCH SIMPLE`; dòng tự tham chiếu được); xoá cha chạy `ON DELETE`: `CASCADE` xoá con (đệ quy, sâu tối đa 32), `SET NULL` đặt NULL các cột con (cột `NOT NULL` thì `23502`), `NO ACTION`/`RESTRICT` báo `23503`; sửa cột được tham chiếu mà còn con cũng `23503`. Hai dị thường của snapshot isolation (con chèn trong lúc cha bị xoá, và ngược lại) được chặn khi commit giống `FOR KEY SHARE` của PG: committer giữ hai map nhỏ trong bộ nhớ, "đã được dùng làm cha tại LSN" (con chèn/sửa) và "bị xoá hoặc đổi khoá tại LSN" (cha); một giao dịch con có cha bị xoá sau snapshot của nó, hoặc một giao dịch xoá cha có con thêm sau snapshot, nhận `ErrConflict` và làm lại sẽ thấy kết quả đúng (`23503` hoặc xoá lan). Hai con cùng cha không xung đột nhau. Map được dọn theo horizon GC. Bỏ qua kiểm tra này khi ghi bằng `Engine.Write` (API thô, không qua giao dịch).
`ALTER TABLE ADD FOREIGN KEY` chặn ghi vào cả hai bảng, kiểm từng dòng, dựng index nội bộ rồi ghi DDL. `DROP TABLE` bảng đang được tham chiếu: `2BP01`, hoặc `CASCADE` bỏ các khoá ngoại; `TRUNCATE` bảng được tham chiếu: `0A000`. `DROP COLUMN` bỏ index và khoá ngoại dùng cột đó (đang bị bảng khác tham chiếu thì `2BP01`); `RENAME COLUMN` sửa định nghĩa (đang bị bảng khác tham chiếu thì `0A000`); `RENAME TO` bảng đang được tham chiếu: `0A000`.

**Planner** (`indexpath.go`). Từ các mệnh đề `AND` ở mức cao nhất của `WHERE`: bằng nhau trên tiền tố cột index, rồi một `IN` hoặc một khoảng (`<`, `<=`, `>`, `>=`, `BETWEEN`) trên cột kế. Hằng phải cùng loại với cột (hoặc chuỗi phân tích được); không thì bỏ qua điều kiện đó. Index khớp nhiều cột hơn thắng; không có mô hình chi phí và không có thống kê, nên điều kiện khớp phần lớn bảng vẫn đọc qua index (chậm hơn quét): với aggregate chỉ dùng index khi có `=`/`IN`, còn khoảng thì giữ quét song song. `ORDER BY ... [LIMIT]` đọc theo thứ tự index, không sắp xếp, khi cột `ORDER BY` đi ngay sau tiền tố bằng nhau, cùng chiều, NULL mặc định; dừng sớm khi đủ `OFFSET+LIMIT` dòng. Dùng cho `SELECT` một bảng, `UPDATE`, `DELETE` (cùng đường `scanRows`). **Chưa dùng** cho: join và truy vấn qua bộ thực thi chung (`runQuery`), giao dịch đã ghi vào bảng đó (đọc được ghi của chính nó), giao dịch serializable (quét vẫn bị từ chối, P2.3).

**Catalog.** `pg_class` (`i`), `pg_attribute`, `pg_index`, `pg_indexes`, `pg_constraint` (`u`, `f`, `confdeltype`, `conindid`), `pg_get_indexdef`, `pg_get_constraintdef`, `regclass` tên index, `information_schema.table_constraints`, `key_column_usage`, `referential_constraints`. Chưa có: `pg_depend` cho index phụ, `constraint_column_usage` cho UNIQUE/FK, `COMMENT ON`. Index nội bộ của khoá ngoại không hiện.

**Khác PG.** Không có ràng buộc hoãn; `ON UPDATE` chỉ `NO ACTION`/`RESTRICT`; khoá chính ghép được (§24), khoá ngoại trỏ vào khoá chính ghép được; `UPDATE` khoá chính của bảng được tham chiếu: `0A000`; `INSERT ... ON CONFLICT` chỉ theo khoá chính (vi phạm UNIQUE khác báo `23505`); `CREATE INDEX` chặn ghi vào bảng; ràng buộc/index áp dụng ngay (như các DDL khác, kể cả trong `BEGIN`); nhiều hành động trong một `ALTER TABLE` có ràng buộc chạy từng cái (không nguyên tử); thông báo lỗi gộp `DETAIL` vào thân.

**Tài liệu tham khảo (thiết kế theo cách PG làm; chưa đọc lại mã nguồn trong phiên này, chỉ dựa trên tài liệu và README):** [btree README](https://github.com/postgres/postgres/blob/master/src/backend/access/nbtree/README) và [B-Tree Implementation](https://www.postgresql.org/docs/16/btree-implementation.html) (entry trùng được sắp theo con trỏ heap; index không mang thông tin nhìn thấy được, dòng heap quyết định); [CREATE INDEX](https://www.postgresql.org/docs/16/sql-createindex.html) (khoá ghi, `CONCURRENTLY`); [Explicit Locking](https://www.postgresql.org/docs/16/explicit-locking.html) (`FOR KEY SHARE` dùng cho khoá ngoại); [Index-only scans](https://www.postgresql.org/docs/16/indexes-index-only-scans.html) (recheck dòng heap).

**Đo chi phí ghi** (2026-10-29, `BenchmarkIndexedWrite` trong `internal/core/secindex_bench_test.go`: `go test ./internal/core -run XXX -bench IndexedWrite -benchtime 2s -count 3 -cpu 4`). Giao dịch 50 dòng (một giao dịch 1 dòng chỉ đo fdatasync), bảng 6 cột (khoá chuỗi, 5 cột int), 8 goroutine ghi, mỗi index một cột int. UPDATE đổi giá trị của mọi cột có index (nên vừa thêm entry vừa đánh dấu chết). **Máy dùng chung với ~5 agent khác, load average ~10, đĩa đầy 97%: chỉ đọc là tương đối**, 3 lượt cho mỗi ô, µs/dòng:

| số index | INSERT | UPDATE |
|---|---|---|
| 0 | 11,5 · 14,9 · 11,2 | 11,5 · 10,8 · 12,6 |
| 1 | 9,9 · 10,3 · 9,6 | 14,0 · 10,0 · 8,9 |
| 3 | 23,2 · 34,1 · 32,9 | 25,9 · 27,8 · 30,2 |
| 5 | 41,5 · 25,5 · 27,0 | 39,6 · 39,1 · 38,3 |

Một index nằm trong nhiễu; 3 index làm ghi chậm khoảng 2,5 lần và 5 index khoảng 3 lần (≈ +6–8 µs/dòng/index). **Chưa đạt mục tiêu "không làm xấu"** cho bảng có nhiều index. Nguyên nhân dự đoán (chưa profile): mỗi entry mới vào một lá ngẫu nhiên của cây copy-on-write, nên mỗi lần `put` chép một lá 2,7 KB (và nút trong) rồi để GC dọn, và việc này chạy tuần tự trong committer. Hướng xử lý: (1) cho `flushAdds` chạy song song với fdatasync của chính batch (cần giữ các khoá UNIQUE của batch đang bay), (2) sửa tại chỗ trong lá thay vì chép (cần khoá đọc/seqlock), (3) sắp xếp entry của batch theo khoá để nhiều entry dùng chung một lần chép lá. Bảng không có index (so với commit gốc `8e48b23`, cùng bench, xen kẽ 2 vòng × 2 lượt, 0 index): INSERT 13,2/21,9/21,4/23,6 µs (gốc) so với 13,4/12,7/23,8/27,7 (mới); UPDATE 21,1/23,2/22,8/21,4 so với 22,1/22,2/26,8/25,7. Nhiễu của máy (load ~10) lớn hơn chênh lệch; không thấy khác biệt rõ nhưng cũng không loại trừ được vài chục phần trăm. Chưa chạy WL-1 đầy đủ theo docs/11 §5 (không ghim CPU được trên máy dùng chung).

**Cập nhật `commitfair` (2026-10-29, D25).** Profile cho thấy `flushAdds` clone cây index một lần cho mỗi entry (entry của một giao dịch xen kẽ giữa các index nên mỗi lần `put` lại chép gốc và nút trong), chiếm ~31% thời gian một lượt; `markDead` ~16% và giữ `si.mu` cả lượt; GC ~20%. Sửa: `flushAdds` gom theo index, sắp theo khoá, một clone mỗi index và chạy song song giữa các index; `markDead` theo lát 64; giao dịch lớn dàn entry vào cây theo lát trước khi đặt LSN (`commitstage.go`, xem D25). Chi phí ghi sau khi sửa (`BenchmarkIndexedWrite`, xen kẽ gốc/mới, µs/dòng, 2 lượt): INSERT 0 idx 9,2 → 9,1; 3 idx 20-21,5 → 10,8-13,2 (2,2× → 1,2-1,4×); 5 idx 26-40 → 12,7-14,3. UPDATE 0 idx 8,4-9,8 → 9,5-9,7; 3 idx 26-28,6 → 15,3-16,3 (2,8-3,0× → 1,6-1,7×); 5 idx 37-42 → 17,8-20 (≈2×, chưa đạt 1,8×). Hướng (2) sửa tại chỗ trong lá và bớt chép nút trong (hai mảng con trỏ `leaves`/`inners` của `innerNode` chỉ dùng một) chưa làm. Entry tạm của giao dịch lớn thua được xoá khi huỷ; trường hợp hiếm còn rò (khoá cùng giá trị bị đổi rồi giao dịch lớn thua) chỉ làm một entry thừa tới khi khởi động lại, không sai kết quả (scan luôn kiểm lại hàng).

**Test.** `internal/core/secindex_test.go` (xây, duy trì, snapshot cũ, thứ tự xuôi/ngược, khoảng, NULL, sweep, khởi động lại bằng replay và bằng checkpoint, DROP, UNIQUE kể cả đồng thời), `internal/core/fk_test.go` (các hành động, cha con cùng giao dịch, đua chèn con với xoá cha, có đột biến kiểm chứng test bắt được khi tắt `checkFK`), `internal/sql/index_test.go` (cùng truy vấn trước và sau khi tạo index phải ra cùng kết quả và đọc index thật, SQLSTATE, đồng thời, catalog), `internal/repl/secindex_repl_test.go` (replica).


## 22. Tìm kiếm khớp mẫu trong SQL: chỉ mục trigram (nhánh `search`, 2026-10-29)

**Mục đích.** Bài toán tra cứu số SIM (docs/15): tiền tố, đuôi, chứa, số đẹp, kèm bộ lọc và đếm theo nhóm, trước đây phải đẩy sang Elasticsearch. Nay chạy trong SQL, trên một view dẫn xuất từ cùng log.

**Khai báo (tương thích PostgreSQL).**

```sql
CREATE INDEX sim_num ON sim USING gin (num gin_trgm_ops);   -- cú pháp pg_trgm; cũng nhận gist/gist_trgm_ops
CREATE INDEX sim_num ON sim USING search (num);             -- hoặc USING trigram (num)
CREATE TABLE sim (...) WITH (trigram = 'num');              -- hoặc thuộc tính bảng
DROP INDEX sim_num;
```

Mọi `USING` khác (btree) được trả lại cho trình phân tích CREATE INDEX khác (nhánh `index`): `createSubstrIndex` chỉ nhận khi phương thức là gin/gist/search/trigram và **không tiêu thụ gì** nếu không phải. Khi gộp với nhánh `index`, `DROP INDEX name` của hai nhánh cần hợp nhất (ở đây `dropSubstrIndex` chỉ biết chỉ mục trigram; nếu không thấy tên thì báo 42704; nhánh `index` phải gọi nó trước và chuyển tiếp phần còn lại). Khai báo lưu trong `schema.Table.Substr` (một trường mới) và đi theo DDL qua WAL; `CREATE INDEX` trên bảng đã có dữ liệu dựng lại view từ ảnh chụp tại LSN của DDL (2M dòng: 10,5 s). Không cho DROP/RENAME cột đang có chỉ mục (RENAME cập nhật khai báo).

**Câu truy vấn dùng chỉ mục** (là các điều kiện AND ở mức cao nhất của WHERE, trên cột có chỉ mục, hằng số hoặc tham số; chỉ SELECT một bảng, ngoài giao dịch tường minh):

| Dạng | Cách dùng |
|---|---|
| `col LIKE/ILIKE 'mẫu'` | trigram của các đoạn chữ giữa `%` và `_` (có escape `\`), giao các danh sách; mẫu không có đoạn ≥3 ký tự (`'%88'`) thì view tự duyệt giá trị trong bộ nhớ |
| `col ~ 're'`, `~*` | các chuỗi ký tự bắt buộc rút từ cây regex (`regexp/syntax`), view kiểm bằng chính regexp của Go |
| `sim_pretty(col, 'quad'\|'triple'\|'asc'\|'repeat'\|'palin')` | cờ lớp số đẹp lưu theo từng giá trị: tứ quý (4 số cuối giống nhau), tam hoa (3), tiến (4 số cuối tăng 1: 1234), lặp (`xyxy`, x≠y, hoặc `xyzxyz`), gánh (`abba` a≠b hoặc `abccba`). Là hàm SQL thường, nên cũng chạy được khi quét |
| `NOT LIKE`, `!~`, `OR` | không dùng chỉ mục (quét) |

Ghép nhiều điều kiện trên cùng cột thì gộp (giao trigram, AND kiểm tra). Bộ lọc khác (`carrier`, `status`, khoảng giá), `GROUP BY` (facet), `ORDER BY`, `LIMIT` do bộ thực thi thường xử lý trên các dòng ứng viên; mỗi ứng viên được đọc tại snapshot của câu lệnh và **toàn bộ WHERE được tính lại**, nên không bao giờ trả dòng không khớp hay giá trị cũ. `LIMIT` không `ORDER BY` đọc ứng viên theo lô và dừng sớm.

**Hợp đồng nhất quán** (`internal/planner/substr.go`, dùng lại `choose`):

```sql
SET stask.consistency = 'strong' | 'bounded' | 'session' (mặc định, D26) | 'eventual';
SET stask.max_lag_ms = 1000;      -- ngưỡng cho bounded
```

- `strong`: view phải tới LSN bền (chờ ≤2 s) rồi mới trả lời, không kịp thì **rơi về quét lõi**.
- `bounded`: view trễ ≤ ngưỡng thì dùng ngay; trễ hơn thì chờ tối đa ngưỡng, không kịp thì quét lõi. Trong ngưỡng, dòng mới ghi có thể chưa thấy (đã công bố độ trễ); dòng đã sửa/xoá không bao giờ hiện giá trị cũ vì mỗi ứng viên đọc lại từ lõi.
- `session` (**mặc định từ nhánh `sqlgaps`, D26**): view phải tới LSN commit cuối của chính phiên (`stask.last_lsn`) hoặc `stask.min_lsn` nếu lớn hơn; chờ tối đa 500 ms rồi rơi về quét lõi (vẫn thấy dòng của mình). Phiên chưa ghi gì thì đọc view nguyên trạng. Khác bản cũ (`max(snapshot, min_lsn)`): không chờ ghi của phiên khác. Đo `simfresh` 1M, đang nhập: `insert/default` thấy ngay dòng vừa ghi 175/285 (61%) -> 328/328 (100%); p50 0,65 -> 0,86 ms, p99 8,3 -> 18,0 ms (một lần chạy, máy dùng chung). Phạm vi là *phiên* (kết nối): pool chia truy vấn ra nhiều kết nối thì không có bảo đảm giữa các kết nối.
- `eventual`: dùng nguyên trạng.
- Có tồn tại khoảng "mất dòng mới" duy nhất: dòng mới/được sửa thành khớp mà view chưa nhận, trong ngưỡng bounded hoặc ở eventual. Với strong thì không.

**CREATE INDEX chờ dựng xong (nhánh `sqlgaps`).** `CREATE INDEX .. gin_trgm_ops` chỉ trả khi view đã áp DDL và dựng chỉ mục (chờ tối đa `SubstrBuildTimeout`=30 phút, huỷ được; hết hạn thì trả kèm chỉ mục tiếp tục dựng ngầm). `CREATE INDEX CONCURRENTLY` trả ngay như cũ; trong lúc dựng, `EXPLAIN` hiện `Seq Scan` kèm `reason="trigram index X on t(c) is still being built ..."` và truy vấn quét lõi.

**EXPLAIN mọi câu.** `EXPLAIN [ANALYZE]` nhận SELECT, INSERT, UPDATE, DELETE, WITH (kể cả CTE ghi), UNION; hiện `Index Scan using t_pkey`, `Index Scan using <btree>` (kể cả quét theo thứ tự thay Sort), `Trigram Scan` (kèm applied_lsn/view_lag_ms), `Seq Scan`, `Hash Join`, `Hash Semi/Anti Join` (EXISTS/IN đã giải tương quan), HashAggregate, Sort, Limit; dùng chính các hàm chọn đường của bộ thực thi. `ANALYZE` chạy thật (ghi thật; bọc BEGIN..ROLLBACK để hoàn tác), báo dòng, thời gian, spill, dùng view cột. Bộ thực thi chung (JOIN, truy vấn con) đọc cả bảng, không đẩy chỉ mục (nói rõ trong kế hoạch). Không có ước lượng chi phí (không thống kê). Có tham số `$n`.

**Quan sát.** `EXPLAIN SELECT ...` (một bảng) trả `Trigram Scan on t using idx`, điều kiện, chế độ và dòng chi tiết: `engine=substr|core mode= applied_lsn= view_lag_ms= waited_ms= reason=`. Sau mỗi SELECT có chỉ mục: `SHOW stask.last_plan` cho cùng nội dung (kể cả khi đã rơi về quét: `Seq Scan on t (trigram index .. not used) engine=core .. reason="view lag exceeds .."`). `/v1/views` liệt kê thêm view `substr` với applied/lag.

**Giới hạn.** Bộ nhớ: giá trị và danh sách trigram nằm trong RAM (đo: khoảng 145 MB heap cho 1M số 10 chữ số, tức 2M ≈ 290 MB; ngoại suy tuyến tính 50M ≈ 7 GB, chưa đo ở cỡ đó). Mỗi UPDATE sinh mã tài liệu mới; danh sách được dựng lại khi chết > sống (≥100k) dưới khoá bảng. Mẫu <3 ký tự: duyệt toàn bộ giá trị trong view (nhanh hơn quét lõi nhưng chậm hơn pg_trgm). Không có `search()`/`@@` (BM25) trong SQL, không có chỉ mục btree cho tiền tố trên nhánh này, không `EXPLAIN ANALYZE` thật (chỉ kế hoạch), không dùng trong JOIN/UPDATE/DELETE. Chỉ mục trigram không bảo đảm duy nhất và không có thứ tự.

## 23. Lịch sử phiên bản trong SQL: AS OF, stask_history, history_retention (2026-10-29, nhánh `history`)

Log đã chứa mọi phiên bản của mọi dòng (docs/03 §2, §8). Mục này cho SQL đọc chúng. Chọn cú pháp rẻ nhất: **không có bảng ảo `t_history`** (một bảng ảo như vậy buộc phải đẩy điều kiện `key = ...` xuống engine để khỏi quét cả lịch sử, mà bộ thực thi chung chưa có cơ chế đẩy xuống). Thay vào đó là một mệnh đề cho bảng và một hàm trả bảng.

```sql
-- đọc cả bảng tại một thời điểm (đọc nhất quán, như một snapshot)
SELECT * FROM sim FOR SYSTEM_TIME AS OF '2026-09-01 12:00:00';   -- SQL:2011 / MariaDB / SQL Server
SELECT * FROM sim AS OF TIMESTAMP now() - interval '1 day';      -- cùng nghĩa
SELECT * FROM sim AS OF LSN 123456;                              -- một vị trí trong log
SELECT status FROM sim AS OF TIMESTAMP '2026-09-01' WHERE number = '0901234567';   -- đọc điểm

-- mọi phiên bản còn giữ của một khoá, cũ nhất trước
SELECT _ts, status, price FROM stask_history('sim', '0901234567');
SELECT * FROM stask_history('sim', '0901234567') WHERE _ts >= now() - interval '7 days';

-- chính sách giữ lịch sử, theo bảng
ALTER TABLE sim SET (history_retention = '60 days');   -- 36h, 60d, '8 weeks' cũng được
ALTER TABLE sim RESET (history_retention);             -- về mặc định toàn cục (-history-retention)
CREATE TABLE sim (...) WITH (history_retention = '60 days');
```

- **Mệnh đề AS OF** đứng ngay sau tên bảng, trước bí danh (`FROM sim AS OF LSN 5 s`). Biểu thức được tính một lần cho mỗi lần chạy (`now()`, tham số `$1`, literal, phép trừ interval đều được). `NULL` là lỗi. Chỉ dùng được cho bảng (không cho view, CTE, bảng hệ thống: `0A000`).
- **Đọc điểm.** Nếu `WHERE` có `khoá = hằng` thì chỉ đi theo chuỗi phiên bản của khoá đó (một `GetAt`/`GetAtTime`); các điều kiện khác vẫn lọc kết quả. Không có thì **quét cả bảng và dựng từng dòng ở thời điểm đó**. Trên 40M dòng đó là quét 40M chuỗi: chấp nhận được cho phân tích hiếm, không dùng cho đường nóng.
- **AS OF TIMESTAMP chính xác.** Thời điểm được đổi thành khoảng LSN `[Lo, Hi)` nhờ các mốc (thời gian, durable LSN) cách nhau ≥10 s (`internal/core/history.go`: `TimeSnapshot`); mọi commit ≤ Lo chắc chắn trước thời điểm, mọi commit ≥ Hi chắc chắn sau. Các phiên bản nằm giữa được xếp theo commit time của chính chúng, nên kết quả đúng đến từng commit (test `TestTimeSnapshot`: lấy mẫu mọi vị trí trong khoảng 400 s giữa hai mốc). Mốc lưu ở `gc-marks.bin` và dựng lại từ bản ghi đầu mỗi segment nếu mất file.
- **Lịch sử đã hết hạn.** Đọc ở thời điểm/LSN thấp hơn phần còn giữ cho `72000` (HTTP 410), không bao giờ trả thiếu âm thầm. `stask_history` chỉ liệt kê phần còn giữ: **không có cách phân biệt "khoá chưa từng có phiên bản cũ hơn" với "đã bị thu hồi"** nếu nhìn riêng kết quả; so `min(_ts)` với `now() - history_retention`.
- **stask_history** trả `_lsn bigint, _ts timestamptz, _deleted bool` rồi các cột của bảng (NULL nếu là bản xoá). Tên bảng phải là hằng chuỗi (để biết cột lúc biên dịch). Cần quyền `SELECT` trên bảng. Giá: đi theo chuỗi `prev` trong log, một lần đọc cho mỗi phiên bản (đo ở docs/15 §7.4).
- **history_retention** là thuộc tính schema (đi qua WAL, checkpoint, replica như mọi `ALTER TABLE`), nên một replica biết nó. `ALTER` yêu cầu quyền như các `ALTER TABLE` khác. Mặc định toàn cục: `staskd -history-retention 60d` (chấp nhận `36h`, `60d`, `"60 days"`, `8w`); **replica phải chạy với cùng giá trị** vì nó tự quyết định phần log nào còn giữ khi dọn.
- `GET /v1/status` (trường `gc`, cũng là `GCStats`) hiện `history_retention`, `table_history_retention`, `time_marks`, `remapped_versions`.

**Chưa làm.** `AS OF` cho view, `BETWEEN ... AND ...`/`FROM ... TO ...` của SQL:2011 (dùng `stask_history` + `WHERE _ts`), `CURRENT_TIMESTAMP` thay vì thời gian tường của `AS OF` trong giao dịch (mọi `AS OF` đọc ngoài snapshot của giao dịch, không thấy thay đổi chưa commit của chính nó), bảng ảo `t_history`.

**Kiểm chứng.** `internal/sql/asof_test.go` (`TestSQLAsOf`: AS OF LSN/TIMESTAMP/FOR SYSTEM_TIME, đọc điểm, join với trạng thái hiện tại, `stask_history` kể cả khoá đã xoá, ALTER/RESET, lỗi), probe `claimcheck` `history_*`, `as_of_system_time`.

## 24. Bảng không có khoá chính và khoá chính ghép (2026-10-29, nhánh `nopk`)

**Vì sao.** Liquibase tạo `DATABASECHANGELOG` không có khoá chính, và changelog Jmix/Hibernate dùng `PRIMARY KEY (a, b)` cho bảng liên kết. Trước đó `CREATE TABLE` đòi đúng một cột khoá (`0A000`), nên mọi migration dừng ở bước đầu.

**Thiết kế (không đổi định dạng dòng hay log).** Mỗi bảng vẫn có đúng một *cột khoá nội bộ* (`Table.Key`); chỉ cách chọn nó thay đổi:

| Bảng khai báo | Cột khoá nội bộ | Giá trị khoá |
|---|---|---|
| một cột `PRIMARY KEY (id)` | chính cột đó (như cũ) | như cũ (int: số thập phân; string/uuid: chuỗi) |
| không có `PRIMARY KEY` | cột **ẩn** `__rowid` (`int`, `NOT NULL`) | `nextval` của sequence sở hữu `<bảng>____rowid_seq` (là `DEFAULT` của cột, nên `INSERT`, `COPY`, `INSERT ... SELECT` đều điền được) |
| `PRIMARY KEY (a, b, ...)` | cột **ẩn** `__pk` (`string`) | mã hoá có thứ tự của các cột khoá, đúng `AppendKeyValue` ở `schema/indexkey.go` (prefix-free, so byte = so tuple), nối theo thứ tự khai báo |

Cột ẩn được lưu như mọi cột (đi qua log, checkpoint, replica, view dẫn xuất không đổi). Với `__pk`, `Table.KeyOf` tính khoá **từ các cột khoá**, và `Table.Encode` ghi giá trị đó vào cột ẩn, không bao giờ lấy từ người gọi: cột ẩn không thể lệch khỏi cột thật. `UPDATE` đổi cột khoá ghép là xoá + chèn như đổi khoá chính một cột (cùng ràng buộc với khoá ngoại tham chiếu, `0A000`).

**Bền vững.** `__rowid` lấy từ sequence nên thừa hưởng tính chất sequence (§2): nhật ký trước (một lần ghi log cho 32 giá trị; riêng sequence của `__rowid` cho 65536, vì mỗi dòng nạp hàng loạt lấy một giá trị: với 32, nạp 1000 dòng vào bảng không PK chậm 5,5 lần), sống qua restart/checkpoint/replica, không cấp hai lần; sau crash có thể thủng tối đa 65536 số (không ai thấy), không trùng. Replica không sinh giá trị: nó áp bản ghi của leader (cột ẩn nằm trong dòng). Sequence là dòng của bảng hệ thống nên đi cùng log; **thăng cấp replica chưa được thử riêng cho tính chất này** (chỉ kiểm restart leader).

**Ẩn với client.** Không có trong `SELECT *`/`t.*`, `RETURNING *`, `INSERT` không liệt kê cột (số giá trị khớp cột thật), `COPY` (cả hai chiều), `information_schema.columns`, `pg_attribute` (`attnum` của cột thật vẫn từ 1, `relnatts` đúng), `pg_publication_tables`, JSON của HTTP API, CDC (test_decoding, wal2json 1 và 2, pgoutput). Sequence `__rowid` không có trong `pg_class`/`pg_sequences`/`information_schema.sequences`. Tên `__rowid`, `__pk` bị dành riêng (`42939`). Có thể viết rõ tên `__rowid` trong truy vấn (như `ctid`), `SELECT *` không bao giờ trả nó. Bảng không PK không có hàng trong `pg_index`/`pg_constraint`/`pg_indexes`/`table_constraints`, như PG.

**CDC và replica identity (chọn: FULL).** PG với bảng không PK chỉ phát `INSERT` (UPDATE/DELETE bị từ chối nếu không có `REPLICA IDENTITY`). STASK chọn **REPLICA IDENTITY FULL ngầm** cho bảng không PK **và** bảng khoá ghép: `UPDATE`/`DELETE` luôn mang cả dòng cũ (không có cột ẩn), `pg_class.relreplident = 'f'`, thông điệp `R` của pgoutput đặt cờ khoá cho mọi cột. Với khoá ghép, PG dùng identity mặc định (chỉ các cột khoá); STASK phát cả dòng cũ, một tập lớn hơn, và `pk` của wal2json liệt kê đủ cột khoá. Đổi khoá ghép trong `UPDATE` phát `DELETE` rồi `INSERT` (như đổi khoá chính hiện nay). Hạn chế: nếu phiên bản cũ của dòng không đọc được (`OldUnknown`), bảng này phát danh tính rỗng thay vì khoá (không có khoá hiển thị để phát).

**Khoá ghép.** Cột khoá phải là kiểu index được (`int`, `float`, `string`/`text`, `time`, `bool`) và tự thành `NOT NULL`. `ON CONFLICT (a, b)` (đích luôn là khoá chính). `FOREIGN KEY (x, y) REFERENCES t (a, b)` (thứ tự cột tuỳ ý) tra thẳng bằng khoá ghép, không qua index phụ. `pg_index.indkey = '1 2'`, `pg_constraint.conkey = {1,2}`, `pg_get_constraintdef = PRIMARY KEY (a, b)`, `key_column_usage` theo thứ tự. Thông điệp trùng khoá: `Key (a, b)=(1, x) already exists`. Tìm theo `WHERE a = 1 AND b = 'x'` hiện **quét bảng** (đường tra khoá điểm của planner chỉ biết khoá một cột); tạo index phụ trên (a, b) để có đường index. Không cho bảng `columnar` (`0A000`).

**`ALTER TABLE ... ADD [CONSTRAINT n] PRIMARY KEY (cols)`** chỉ khi bảng chưa có khoá chính **và không có dòng nào** (mọi dòng sẽ phải đổi khoá: chưa làm, `0A000`); cột khoá một cột phải là `int`/`varchar`/`uuid` (không phải `text`). Cột `__rowid` bị bỏ (như DROP COLUMN) cùng sequence của nó; khoá ghép thêm cột `__pk`. Cột thuộc khoá chính không `DROP COLUMN`/`ALTER TYPE`/`DROP NOT NULL` được.

**Giới hạn đã biết.** Tên constraint khai báo (`CONSTRAINT pk_x`) vẫn bị thay bằng `<bảng>_pkey` (như trước). Bảng không PK: `ON CONFLICT DO NOTHING` không bao giờ thấy xung đột (PG báo `42P10` nếu không có ràng buộc để so); HTTP API ghi dòng (`FromJSON`) từ chối bảng không PK (dùng SQL). Một bản Go cũ đọc được JSON bảng có trường mới nhưng không biết ẩn cột.

**Kiểm chứng.** `internal/sql/nopk_test.go` (không PK: trùng dòng, `*`, UPDATE/DELETE, catalog, TRUNCATE; khoá ghép: trùng, ON CONFLICT, đổi khoá, FK, catalog; ADD PRIMARY KEY; CDC), `nopk_prop_test.go` (so với mô hình map qua checkpoint và restart: 6 seed x 300 bước mỗi loại), `internal/repl/nopk_repl_test.go` (replica = leader qua ghi ngẫu nhiên, restart replica, checkpoint leader), `internal/schema/pkey_test.go` (thứ tự mã hoá, giải mã, JSON cộng thêm), probe `claimcheck` `nopk_*`, `composite_pk_*`, `alter_add_primary_key`, `pd_table_without_pk`, `pd_composite_pk`, `lb_changelog_*`; `scripts/pgdiff/e_liquibase_run.sql` (nhóm e, 74 câu: khởi động Liquibase, 4 changeSet, truy vấn metadata). Số đo: docs/14 Phụ lục D.


## 25. Toán tử và hàm json/jsonb và mảng (nhánh `jsonarr`, 2026-10-30)

Mã: `internal/sql/jsonops.go` (cây JSON, toán tử, hàm), `jsonpath.go` (jsonpath nhỏ), `arrayops.go` (mảng), `jsonarr.go` (nối vào parser/evaluator/planner, aggregate, SRF, OID mảng, chỉ mục GIN), `schema/gin.go`. Định dạng đĩa **không đổi**: mảng vẫn là kiểu cột 14 (dạng văn bản PG), json 12, jsonb 13 (docs/09 §4). Thêm duy nhất: trường schema JSON `gin_indexes` (`omitempty`) và `Column.PG` nhận thêm các giá trị kết thúc `[]` (`int4[]`, `int2[]`, `varchar(5)[]`, `date[]`, ...) để nhớ kiểu phần tử; bảng cũ không có `pg` đọc như trước (OID `int[]` cũ vẫn là `_int8`).

**Cú pháp mới.** Lexer: `->>`, `#>>`, `#>`, `#-`, `@>`, `<@`, `@?`, `@@`, `?|`, `?&`, `&&`. Nhóm "toán tử khác" của PG (`||`, `->`, `->>`, `#>`, `#>>`, `@>`, `<@`, `&&`, `?`, `?|`, `?&`, `@?`, `@@`, `#-`) nằm giữa `+ -` và so sánh, kết hợp trái. **Đổi hành vi có chủ đích:** `||` trước đây ngang hàng `+ -`; nay lỏng hơn (đúng PG: `'a' || 1 + 2` là `'a3'`). Chỉ số `a[i]`, `a[i][j]`, lát cắt `a[lo:hi]`, `a[:hi]`, `a[lo:]`. `unnest(a, b, ...) [WITH ORDINALITY]` và `jsonb_each(...)` ở FROM, kể cả LATERAL (`FROM t, jsonb_array_elements(t.j) e`): viết lại thành truy vấn con LATERAL có các SRF trong danh sách chọn (cơ chế SRF sẵn có, chạy song song, đệm NULL). Cột của hàm một cột lấy tên bí danh (`... e` rồi dùng `e`; `e.value` **không** dùng được, khác PG cho phép cả hai).

**json/jsonb.** `-> ->> #> #>>` (chỉ số âm, đường dẫn `text[]`), `@> <@ ? ?| ?& || - #-`, `@?` `@@` và `jsonb_path_exists/match/query/query_first/query_array` (jsonpath lax/strict: `$ @ .key .* [n] [a to b] [*] ?(...)`, `== != <> < <= > >=`, `&& || !`, `exists()`, `.size() .type() .double() .abs() .floor() .ceiling()`; **không có** biến `$name`, `like_regex`, `starts with`, số học, hàm ngày), `jsonb_build_object/array`, `json_build_*` (định dạng `{"a" : 1}`), `to_json[b]`, `jsonb_set`, `jsonb_set_lax`, `jsonb_insert`, `jsonb_each[_text]`, `jsonb_array_elements[_text]`, `jsonb_object_keys` (và bản `json_*`), `jsonb_typeof`, `jsonb_array_length`, `jsonb_strip_nulls`, `jsonb_pretty`, `json[b]_extract_path[_text]`, aggregate `json_agg` (có `",\n "` giữa các phần tử mảng, như PG), `jsonb_agg`, `json[b]_object_agg` (kể cả `ORDER BY`, `DISTINCT`, `FILTER`). `json` giữ nguyên văn bản (`->` trả đoạn con nguyên văn); `jsonb` in qua `schema.NormalizeJSONB` (thứ tự khoá, `": "`, `", "`, số kiểu numeric) nên khớp PG từng ký tự trên 218 câu của nhóm h.

**Mảng.** Kiểu cột `smallint/int/bigint/real/double/numeric(p,s)/text/varchar(n)/char/bool/uuid/timestamptz/timestamp/date[]` (nhiều chiều khai báo `[][]` được: chiều không phải một phần của kiểu). Literal `'{...}'` và `ARRAY[..]` nhiều chiều (hình chữ nhật, kiểm tra 22P02/2202E), `ARRAY(SELECT ..)`, ép kiểu `::int4[]`. Kiểm khi ghi: miền `int2/int4`, độ dài `varchar(n)`, `numeric(p,s)` từng phần tử. Toán tử `@> <@ && || = <> < >` (so sánh từng phần tử, NULL cuối), `x op ANY|ALL (mảng)` (NULL đúng ba trị), hàm `array_append/prepend/cat/remove/replace/position/positions/length/ndims/dims/upper/lower/fill/to_string`, `string_to_array`, `cardinality`, `generate_subscripts`, `unnest` nhiều mảng, `array_agg` (`ORDER BY`, `DISTINCT`, `FILTER`; mảng đầu vào thành mảng nhiều chiều). Mảng vẫn lưu và tính trên **văn bản**: mỗi phép đọc phân tích lại chuỗi (O(độ dài)); không có chỉ số phần tử.

**OID.** OID mảng đúng (`_int2 _int4 _int8 _text _varchar _numeric _uuid _timestamptz _bool _float4 _float8 _date ...`) trong RowDescription/Describe cho: cột (qua `Column.PG`), `ARRAY[..]`, ép kiểu, `array_agg`, hàm mảng, `a[i:j]`, `a[i]` (kiểu phần tử), `ARRAY(SELECT)`, `unnest`; `pg_type` thêm `_name _bpchar _float4 _date _time _interval`; `pgoutput` dùng OID mảng của cột (`elemOID` dùng bảng ngược mới). Cột bí danh LATERAL giờ mang kiểu cột ngoài (`OuterRef.K/Ty`): đổi này có phạm vi rộng hơn mảng (kiểu của cột tham chiếu ngoài trong truy vấn con tương quan nay không còn mất).

**GIN.** `CREATE INDEX .. USING gin (cột_jsonb | cột_mảng) [jsonb_ops|jsonb_path_ops|array_ops]` được **chấp nhận và lưu** (`Table.Gin`), `DROP INDEX` bỏ nó, trùng tên báo 42P07. **Không tăng tốc** `@>`/`?`/`&&` (vẫn quét bảng) và chưa hiện trong `pg_indexes`/`pg_class`; một cột, không `WHERE`.

**Chưa làm / sai khác đã biết** (xem docs/14 JA1): tham số mảng nhị phân (pgjdbc `setArray`, pgx `= ANY($1)` với `[]int64`: STASK chưa báo OID mảng cho tham số, probe `ext_param_any_array` vẫn trượt) và kết quả nhị phân của mảng không phải `text[]`/một chiều; `row_to_json(t)`, `jsonb_populate_record`, `json_to_record` (cần giá trị hàng/record); `ARRAY[..] || 'literal'` coi chuỗi là literal mảng (như PG, nhưng cũng áp cho cột text); lát cắt mảng nhiều chiều chỉ theo chiều đầu; thứ tự `<`/`>` của jsonb so văn bản; `pg_typeof` của biểu thức NULL ở đường một bảng không phân tích kế hoạch trả NULL; trong truy vấn đơn bảng `pg_typeof(i4[1])` là kiểu theo giá trị (`bigint`), trong đường kế hoạch là `integer`.

## 26. Con trỏ SQL, khoá mềm (FOR UPDATE, LOCK TABLE, advisory), SET ROLE (nhánh `cursors`, 2026-10-30)

Mã: `internal/sql/cursor.go`, `locks.go`, `rowlock.go`, `advisory.go`, `setrole.go`, `showall.go`; `internal/core/rowlock.go` (hai hàm cộng thêm, không đổi log/đĩa). Test: `internal/sql/cursors_test.go`, `internal/pgwire/cursors_wire_test.go`, `scripts/pgdiff/h_cursors.sql` (256 câu), `cmd/claimcheck/probes_cursors.go` (32 probe), `scripts/cursors-clients.sh` (psycopg2 + pgjdbc).

### 25.1 Con trỏ: `DECLARE`, `FETCH`, `MOVE`, `CLOSE`, `pg_cursors`

`DECLARE name [BINARY] [INSENSITIVE] [[NO] SCROLL] CURSOR [WITH|WITHOUT HOLD] FOR query`; `FETCH|MOVE [NEXT|PRIOR|FIRST|LAST|ABSOLUTE n|RELATIVE n|n|ALL|FORWARD [n|ALL]|BACKWARD [n|ALL]] [FROM|IN] name`; `CLOSE name|ALL`; view `pg_cursors` (`name, statement, is_holdable, is_binary, is_scrollable, creation_time`). Thẻ lệnh `DECLARE CURSOR`, `FETCH n`, `MOVE n`, `CLOSE CURSOR`. Lỗi như PG: `34000` (không có con trỏ), `42P03` (trùng tên), `25P01` (`DECLARE` không `WITH HOLD` ngoài khối giao dịch), `55000` (`NO SCROLL` mà lùi, `FETCH LAST` hay `ABSOLUTE` âm). Extended protocol: `Describe` của `DECLARE` trả tham số của truy vấn, của `FETCH` trả cột của con trỏ (pgx ở cả ba chế độ chạy).

**Đọc từ snapshot lúc `DECLARE`** (đúng `INSENSITIVE`): ghi sau đó trong cùng giao dịch không thấy. Con trỏ sống trong giao dịch: `COMMIT` đóng mọi con trỏ không `WITH HOLD`; `ROLLBACK` (và `COMMIT` của giao dịch đã hỏng) đóng cả con trỏ `HOLD` khai báo trong giao dịch đó. `WITH HOLD`: lúc `COMMIT` phần kết quả còn lại được chép ra bộ nhớ hoặc, quá ngân sách bộ nhớ truy vấn (`-query-mem-mb`), ra tệp tạm của `internal/spill` (cần `SpillDir`, nếu không `COMMIT` lỗi `53200` thay vì mất con trỏ); ngoài khối giao dịch, `DECLARE ... WITH HOLD` chạy truy vấn ngay.

**Phát trực tiếp theo lô hay vật chất hoá.** Dạng truy vấn quyết định:

| Truy vấn | Cách chạy | Bộ nhớ |
|---|---|---|
| một bảng, `WHERE`/danh sách chọn tuỳ ý, **không** `ORDER BY`, tổng hợp, cửa sổ, `DISTINCT`, SRF, `FOR UPDATE`; có thể `LIMIT/OFFSET`; bảng chưa bị chính giao dịch ghi | **phát trực tiếp**: đọc bảng theo thứ tự khoá từng lô 1024 dòng (`Tx.ScanFrom`, không giữ khoá engine giữa hai `FETCH`), mỗi lô đi qua bộ thực thi thường | một lô + cửa sổ 4096 dòng, không phụ thuộc cỡ bảng |
| mọi truy vấn khác (`ORDER BY`, join, nhóm, `VALUES`, `SetOp`, `FOR UPDATE`, bảng đã ghi trong giao dịch) | **vật chất hoá**: chạy một lần trên bộ thực thi chung (có ngân sách `53200` và spill của nó), con trỏ giữ cả kết quả | đỉnh = cỡ kết quả (bộ thực thi chung chưa phát dòng) |

Lùi (`PRIOR`, `BACKWARD`, `ABSOLUTE` nhỏ) trong cửa sổ trả từ bộ nhớ; xa hơn thì đọc lại snapshot từ đầu (snapshot được ghim bởi giao dịch nên cùng kết quả). Thứ tự dòng của con trỏ phát trực tiếp là thứ tự khoá của chỉ mục chính (khoá số là chuỗi thập phân nên `1,10,2,...`), như `SELECT` không `ORDER BY`; PG trả theo thứ tự vật lý. Mặc định (không `SCROLL`/`NO SCROLL`) con trỏ cuộn được, trừ khi có `FOR UPDATE`.

**Số đo bộ nhớ** (`/tmp/memtest.sh`, bảng 1,2 triệu dòng, `psycopg2` con trỏ có tên `itersize=2000` so với `fetchall()` cùng truy vấn, RSS của staskd, `GOGC=10` để loại độ trễ GC): con trỏ **+7 MB** đỉnh (1.113.403 dòng, 3,6 s), `SELECT` thường **+278 MB** (5,5 s). Với `GOGC` mặc định, RSS tăng theo kích thước bộ nhớ đã sống (chỉ mục) chứ không theo kết quả: 600k dòng +107 MB, 1,1M dòng +413 MB cho con trỏ; đây là độ trễ của GC Go, không phải con trỏ giữ dòng (với `GOGC=10` còn +7 MB).

**Chưa có / hạn chế:** `BINARY` được nhận nhưng kết quả vẫn ra định dạng cột client yêu cầu; `ROLLBACK TO SAVEPOINT` không đóng con trỏ mở sau savepoint; `DECLARE ... FOR UPDATE` khoá mọi dòng lúc `DECLARE` (PG khoá khi `FETCH`); `UPDATE/DELETE ... WHERE CURRENT OF` chưa có.

### 25.2 `SELECT ... FOR UPDATE | NO KEY UPDATE | SHARE | KEY SHARE [OF t] [NOWAIT | SKIP LOCKED]`

Trước đây `FOR ...` được phân tích rồi bỏ. Ngữ nghĩa đúng cho OCC (STASK không có khoá hàng trong engine, người ghi không chờ; first-committer-wins) là **bảng khoá mềm trong bộ nhớ + ghi nhận đọc để commit thua**:

1. Các dòng thoả `WHERE`, theo thứ tự `ORDER BY`, lấy cho đến khi đủ `OFFSET+LIMIT` dòng **đã khoá được** được xin khoá mềm `(bảng, khoá chính)` theo ma trận xung đột của PG (hàng: `KEY SHARE < SHARE < NO KEY UPDATE < UPDATE`) và được ghi vào tập đọc của giao dịch (`Tx.LockRead`; bộ commit đã kiểm tập đọc như với `serializable`). Nếu ai đó commit ghi vào dòng đó trước, **`COMMIT` trượt `40001`** (người thua).
2. Dòng đã bị ghi sau snapshot không khoá được: trong `BEGIN` báo `40001` ngay (như PG `REPEATABLE READ`); lệnh đơn ngoài `BEGIN` được thử lại tự động trên snapshot mới (thấy bản mới như `READ COMMITTED`); `SKIP LOCKED` bỏ qua dòng đó.
3. Dòng đang bị giao dịch khác giữ khoá xung đột: **chờ** (huỷ bằng cancel, `statement_timeout`, `lock_timeout` → `55P03`); `NOWAIT` → `55P03` ngay; `SKIP LOCKED` → bỏ qua dòng và **không tính vào `LIMIT`**. Chờ vòng → `40P01` (đồ thị chờ trong `locks.go`).
4. Khoá nhả khi `COMMIT`, `ROLLBACK`, đóng phiên (kể cả `DISCARD ALL`), lệnh đơn xong, và biến mất khi tiến trình chết; không ghi gì ra log, không đổi định dạng đĩa.
5. `UPDATE/DELETE/INSERT` thường **không** nhìn bảng khoá (vẫn lạc quan): người giữ `FOR UPDATE` không chặn người ghi, mà thua ở commit. Mẫu của kho SIM `UPDATE sims SET ... WHERE number IN (SELECT number FROM sims WHERE status='free' ORDER BY number LIMIT n FOR UPDATE SKIP LOCKED) RETURNING number` chạy được và không cấp trùng số (test đồng thời).

`FOR UPDATE OF t` khoá đúng bảng nêu (join: khoá theo từng bảng lá của `FROM`); khoá trên view/CTE/subquery trong `FROM` không có tác dụng. Lỗi `0A000` với `DISTINCT`, `GROUP BY`, tổng hợp, cửa sổ. Đường nhanh một bảng (`selectRows`, vẫn dùng khoá chính và chỉ mục) và bộ thực thi chung (join) đều khoá. Khác PG: một dòng giữ khoá mà ghi bởi người khác chỉ làm hỏng commit của người giữ (không chặn người ghi); `SKIP LOCKED` cũng bỏ qua dòng đã đổi sau snapshot (không đánh giá lại `WHERE` trên bản mới nên có thể trả ít dòng hơn PG, không bao giờ trùng).

### 25.3 `LOCK TABLE`

`LOCK [TABLE] [ONLY] t [, ...] [IN <8 chế độ> MODE] [NOWAIT]`: nhận mọi chế độ của PG; ngoài khối giao dịch `25P01`; bảng không tồn tại `42P01`; kiểm quyền với vai trò. Chế độ ghi vào bảng khoá mềm và xung đột với `LOCK TABLE` của phiên khác theo ma trận PG (chờ/`NOWAIT` `55P03`/`40P01`), nhả khi hết giao dịch. **Không** chặn `SELECT/INSERT/UPDATE/DELETE` của phiên khác (OCC không có khoá bảng thật): chỉ phục vụ ứng dụng dùng `LOCK TABLE` để tuần tự hoá lẫn nhau.

### 25.4 Khoá tư vấn

`pg_advisory_lock[_shared]`, `pg_try_advisory_lock[_shared]`, `pg_advisory_unlock[_shared]`, `pg_advisory_unlock_all`, `pg_advisory_xact_lock[_shared]`, `pg_try_advisory_xact_lock[_shared]`, với `bigint` hoặc hai `int4` (hai không gian khoá riêng, hai `int4` ngoài miền → `42883` như PG). Mức phiên đếm lồng (khoá hai lần phải nhả hai lần), nhả khi đóng phiên; mức giao dịch nhả khi `COMMIT/ROLLBACK`; cùng phiên không bao giờ tự chặn. Chặn có thời hạn bằng `statement_timeout`/`lock_timeout`, phát hiện vòng chờ. Kiểu trả về đúng PG (`bool`, `void` hiện chuỗi rỗng). `pg_locks` liệt kê khoá mềm (loại `advisory`, `relation`, `tuple`).

### 25.5 `SET ROLE`, `current_user`, `session_user`

`SET [SESSION|LOCAL] ROLE r|NONE`, `RESET ROLE`, `SET SESSION AUTHORIZATION r|DEFAULT`, `RESET SESSION AUTHORIZATION`, `SHOW role`, `SHOW session_authorization`. Phiên có người dùng phiên (đăng nhập, đổi bằng `SESSION AUTHORIZATION`) và người dùng hiện tại (`SET ROLE`); quyền kiểm theo người hiện tại. Chỉ siêu người dùng đổi sang vai trò khác (`42501` cho vai trò giới hạn); vai trò không có `22023`. **Sửa lỗi `realclients`:** phiên đăng nhập bằng mật khẩu chung `-pg-password` không có vai trò lưu, `current_user` trước đây ra `stask`; nay ra tên người dùng trong gói khởi động (`app`), kiểu `name` (OID 19) như PG. `RESET ALL` không đụng vai trò (như PG), `DISCARD ALL` đưa về đăng nhập, đóng con trỏ, nhả khoá tư vấn, bỏ câu lệnh `PREPARE` và `LISTEN`.

### 25.6 Còn lại

`SHOW ALL` (3 cột `name, setting, description`), `DISCARD ALL/PLANS/SEQUENCES/TEMP` (trong khối giao dịch `25001`), `DEALLOCATE ALL` đã có từ trước. Vai trò giới hạn trước đây bị `42501` với `DISCARD`, `RESET`, `SAVEPOINT`, `RELEASE`, `ROLLBACK TO` (pooler gửi `DISCARD ALL` hỏng); nay được phép (không cần quyền bảng).

**Kiểm chứng.** `go test -p 2 ./internal/sql ./internal/pgwire` (đủ gói, qua), `-race` cho các test mới (`-count=3`, sạch). `pgdiff` nhóm `h`: **256 câu, PASS 254, PASS-MSG 2 (thông điệp), DIFF 0**. `claimcheck`: 32 probe `cursors` đều qua. pgx (ba chế độ), psycopg2 `stream_results`/`scrollable`/`withhold`, SQLAlchemy `stream_results`, pgjdbc `setFetchSize(500)` + `DECLARE` + `SKIP LOCKED`/`NOWAIT` giữa hai kết nối: `bash scripts/cursors-clients.sh` (cần `~/rc-tools`).

## 27. Lệnh tạo bảng và nạp dữ liệu cơ bản: CTAS, LIKE, bảng tạm, view vật chất hoá, MERGE, TRUNCATE (nhánh `ctas`, 2026-10-30)

Mã: `internal/sql/ctas.go` (cú pháp), `ctas_exec.go` (CTAS, LIKE, view vật chất hoá, TRUNCATE CASCADE), `temptable.go` (bảng tạm), `merge.go` (MERGE). Hook vào file có sẵn chỉ vài dòng mỗi chỗ (`parse.go`, `exec.go`, `catalog*.go`, `query.go`: một `case` trong `compileFrom`). **Không đổi định dạng log hay đĩa**: ba trường mới của `schema.Table` đều `omitempty` (`matview`, `mat_unpopulated`, `unlogged`).

**CREATE TABLE AS / SELECT INTO.** `CREATE [TEMP|UNLOGGED] TABLE [IF NOT EXISTS] t [(c1, c2)] AS <truy vấn> [WITH [NO] DATA]` và `SELECT .. INTO [TEMP] t FROM ..`; nguồn có thể là `SELECT`, `VALUES`, `TABLE x`, UNION. Truy vấn chạy một lần trong giao dịch của câu lệnh, cột của bảng mới lấy từ `Result.Cols`: `int2/int4/int8`, `float4/float8`, `varchar(n)` (typmod `n+4`), `bpchar(n)`, `name`, `date`, `time`, `timestamp(tz)`, `numeric`, `uuid`, `bytea`, `json(b)`, mảng; **`numeric(p,s)` và `varchar(n)` của cột gốc giữ nguyên khi cột ra là bản sao trần của cột bảng** (`SELECT price FROM t`, `*`), còn biểu thức (`price*2`) ra `numeric` không typmod, như PG (đã đối chiếu `atttypid/atttypmod` với PG16). Bảng mới không có khoá chính (có `__rowid` ẩn), cột cho phép NULL. Tag: `SELECT n` (có dữ liệu), `CREATE TABLE AS` (`WITH NO DATA`, `IF NOT EXISTS` bỏ qua). Cột kiểu `interval` bị từ chối (`0A000`, STASK chưa có cột interval). Như `CREATE TABLE`, DDL áp dụng ngay, không hoàn tác theo giao dịch; nếu câu lệnh lỗi sau khi tạo bảng thì bảng bị xoá lại.

**CREATE TABLE (LIKE src [INCLUDING|EXCLUDING ..])**, có thể trộn với cột khác và nhiều `LIKE`: luôn sao tên, kiểu, `NOT NULL`; `INCLUDING DEFAULTS` sao `DEFAULT` (cột SERIAL dùng chung sequence của bảng gốc, như PG; cột identity chỉ sao với `INCLUDING IDENTITY`, tạo sequence mới); `CONSTRAINTS` sao CHECK; `INDEXES` sao khoá chính, UNIQUE và index thường (tên mới); `ALL` = tất cả. Không có `INCLUDING INDEXES` thì bảng mới không có khoá chính.

**Bảng tạm** `CREATE TEMP|TEMPORARY TABLE [ON COMMIT PRESERVE ROWS|DELETE ROWS|DROP]`, `.. AS`, `SELECT INTO TEMP`: riêng theo phiên (phiên khác không thấy, `pg_class` chỉ liệt kê bảng tạm của phiên, lược đồ `pg_temp_<n>`, `relpersistence='t'`), che bảng cùng tên trong public, mất khi phiên đóng, `DROP TABLE` bảng tạm làm bảng public lộ lại. `ON COMMIT DROP/DELETE ROWS` chạy khi `COMMIT` và sau mỗi câu autocommit; `ROLLBACK` xoá bảng tạm tạo trong giao dịch. **Cách làm (thật thà):** bảng tạm là bảng của engine tên `pg_temp_<phiên>#<tên>` đánh cờ `System`; cờ này đã ẩn bảng khỏi tra cứu của client khác, catalog, CDC (logical decoding, publication `FOR ALL TABLES`) và `pgoutput`; chỉ phiên chủ ánh xạ tên thường sang tên engine (`Session.table`, `userTable`). **Các dòng của bảng tạm VẪN được ghi vào log** (và theo base backup/replica sang bản sao, nơi chúng vô hình): engine chưa có bảng không ghi log, làm vậy là đổi đường commit (dừng và báo cáo theo quy ước). Hệ quả: bảng tạm không nhanh hơn bảng thường, và ghi nhiều vào bảng tạm làm log to. Bảng tạm còn sót sau `kill -9` bị quét khi phiên đầu tiên của tiến trình mới tạo bảng tạm (`sweepTemps`); tới lúc đó chúng chiếm chỗ trong catalog nội bộ nhưng vô hình. `UNLOGGED` được chấp nhận và **coi như bảng thường** (ghi log đầy đủ, sống qua restart, khác PG nơi nó bị cắt khi crash); chỉ ghi `relpersistence='u'`. Giới hạn: tên có tiền tố `public.` vẫn đi qua bảng tạm cùng tên (`qualifiedName` bỏ tiền tố); `pg_temp.t` được chấp nhận như `t`; không có `CREATE TEMP VIEW`/`SEQUENCE`; tên engine lộ trong vài thông điệp lỗi (tên ràng buộc, sequence).

**View vật chất hoá.** `CREATE MATERIALIZED VIEW [IF NOT EXISTS] mv [(cols)] AS <truy vấn> [WITH [NO] DATA]`, `REFRESH MATERIALIZED VIEW [CONCURRENTLY] mv [WITH [NO] DATA]`, `DROP MATERIALIZED VIEW [IF EXISTS] ..`. Là một bảng thường mang `Table.MatView` (văn bản truy vấn); `pg_class.relkind='m'`, `pg_matviews` (`ispopulated`), không có trong `pg_tables`/`information_schema.tables`; `INSERT/UPDATE/DELETE` vào nó: `42809`; `DROP TABLE mv`: `42809`; `REFRESH` một bảng thường: `0A000` (như PG). `REFRESH` chạy truy vấn trong một giao dịch rồi **chỉ ghi phần chênh lệch** (so tập đa tuyến của giá trị từng dòng: xoá dòng cũ không còn, chèn dòng mới), nên người đọc thấy trạng thái cũ cho tới khi commit, không bao giờ thấy view rỗng; `CONCURRENTLY` dùng cùng thuật toán và, như PG, đòi một unique index (`55000` nếu không có) và view đã populate. `WITH NO DATA`: `SELECT` báo `55000`. Qua restart, checkpoint và replica như bảng thường (test). Giới hạn: không `CREATE INDEX` trên view chưa populate; `ALTER MATERIALIZED VIEW` chưa có.

**MERGE** (PG15+; `MERGE INTO t [AS a] USING <bảng|(truy vấn)|VALUES> [AS s] ON cond WHEN [NOT] MATCHED [AND cond] THEN {UPDATE SET .. | DELETE | INSERT [(cols)] [OVERRIDING ..] VALUES (..)|DEFAULT VALUES | DO NOTHING}`; tag `MERGE n` đếm dòng đã UPDATE/DELETE/INSERT). Lập kế hoạch như `UPDATE .. FROM`: một truy vấn `source LEFT JOIN target ON cond` trên bộ thực thi chung, kèm mọi biểu thức của các nhánh WHEN (bọc `CASE WHEN matched` để không tính biểu thức trên dòng không dùng nó), rồi mỗi dòng kết quả chịu nhánh WHEN đầu tiên hợp. Một dòng đích bị hai dòng nguồn cùng UPDATE/DELETE: `21000`. Cả lệnh một giao dịch: nếu có ghi xung đột thì autocommit làm lại cả lệnh (cùng cơ chế `UPDATE .. FROM`; cơ chế sửa xung đột theo dòng `occ_repair` chưa có trong `main` nên chưa dùng). **Tăng tốc:** nếu `ON` có `target.pk = X` với `X` chỉ đọc nguồn và PK một cột, chỉ đọc các dòng đích có khoá nằm trong tập giá trị `X` (đọc điểm qua `FromKeyed`), thay vì quét cả bảng đích. Đo (`TestMergePerf`, 200k dòng staging, một nửa trùng, vào bảng 1M dòng PK text, RAM disk, máy chung): **MERGE 3,3 s, `INSERT .. ON CONFLICT DO UPDATE` 2,3 s (1,43×)**; trước khi có đọc theo khoá: 5,6 s (2,5×); phần JOIN quét cả bảng đích 1M mất ~4 s. Chưa có: `RETURNING` (PG17), `WHEN NOT MATCHED BY SOURCE` (PG17), MERGE vào view; điều kiện `ON` không phải `pk = X` vẫn quét bảng đích.

**TRUNCATE** `t1, t2 [RESTART|CONTINUE IDENTITY] [CASCADE|RESTRICT]`: đã có nhiều bảng và `RESTART IDENTITY`; thêm `CASCADE` (mở rộng tới mọi bảng có khoá ngoại trỏ tới, bắc cầu, kèm NOTICE); không `CASCADE` mà bảng bị tham chiếu: `0A000`. **INSERT**: `OVERRIDING USER VALUE` trên cột `GENERATED ALWAYS` sửa (trước đó bị `428C9`); `OVERRIDING SYSTEM VALUE`, `DEFAULT VALUES`, `ALTER COLUMN SET/DROP DEFAULT/NOT NULL` đã đúng, giữ nguyên (có test).

**Kiểm chứng.** `internal/sql/ctas_test.go`, `ctas2_test.go`, `ctas3_test.go`: CTAS/INTO (kiểu, NO DATA, tên cột, trùng), LIKE, bảng tạm (riêng tư, che, ON COMMIT, rollback, không vào CDC), view vật chất hoá, TRUNCATE/INSERT, MERGE (ví dụ, tham số, giao dịch, đồng thời với ghi khác, **property test 25 seed so với mô hình tham chiếu**), restart/checkpoint (`TestCTASRestart`, kể cả bảng tạm mồ côi), replica (`TestCTASReplica`); `-race` sạch cho các test mới. `scripts/pgdiff/h_ctas.sql`: 206 câu, 10 phiên (kết nối riêng, phiên sau kiểm tra thứ riêng của phiên trước đã biến mất), so với PG16: 197 giống hệt, 9 chỉ khác thông điệp, 0 khác. 45 probe `claimcheck` `ct_*` (`cmd/claimcheck/probes_ctas.go`), cùng đạt trên PG16 và STASK. Số liệu và tái lập: docs/14 Phụ lục F.

## 28. Ứng dụng Jmix/Hibernate/Liquibase: khoá và chỉ mục theo kiểu PG, siêu dữ liệu (2026-10-30, nhánh `jmixfix`)

**Vì sao.** Mười phát hiện của client thật (docs/14 Phụ lục F) chặn changelog Jmix: FK và index trên cột `uuid` (`0A000`), `numeric` không index được, cột 3 phần `schema.bảng.cột` làm hỏng reflection SQLAlchemy, `_pg_expandarray` nhiều cột làm hỏng `getPrimaryKeys`/`getIndexInfo` của pgjdbc, `setDate` gửi `'2026-03-04 +00'`, tên PK bị đổi, `BLOB` thành `OID`.

**Đã làm.**
- **Mã hoá khoá index** (`internal/schema/indexkey_types.go`, thêm kiểu, không đổi mã hoá kiểu cũ): `timestamp`/`timestamptz` (và `date`, lưu như timestamp lúc nửa đêm) cùng mã hoá số nguyên 64 bit vi giây; `uuid` 16 byte; `bytea` cùng mã hoá chuỗi có escape (`00`→`00 ff`, kết thúc `00 01`); `numeric` mã hoá thập phân so sánh được (byte lớp: -Inf < âm < 0 < dương < +Inf < NaN, 4 byte số mũ, chữ số `d+1` kết thúc `00`; âm thì đảo bit; `1.5` và `1.50` cùng khoá). `time` là chuỗi (đã index được). `IndexableType` nhận thêm các kiểu này, nên `CREATE [UNIQUE] INDEX`, `UNIQUE`, FK (cả hai phía), và khoá chính ghép dùng được; `decodeKeyValue` đảo ngược được cả kiểu mới (numeric mất scale thừa).
- **Cột 3 phần** `schema.bảng.cột` (`internal/sql/colref3.go`; phần schema không kiểm).
- **`_pg_expandarray`** là hàm trả tập trong danh sách SELECT (`expandarray.go`); thêm `generate_subscripts(vector,1)` (đếm từ 0 với int2vector như PG), `unnest(int2vector)` (đọc `indkey`), bảng rỗng `pg_opclass`; `typinput='pg_catalog.array_in'::regproc` (pgjdbc `getTypeInfo`); `LIKE ... ESCAPE` với mẫu tính toán (SQLAlchemy `get_schema_names`).
- **Hậu tố múi giờ** `'2026-03-04 +00'`, `+07`, `+05:30` ở date/timestamp (`tzlayouts.go`); `date` bỏ qua múi giờ như PG.
- **Siêu dữ liệu**: giữ tên `CONSTRAINT pk_x PRIMARY KEY` (trường JSON mới `pk_name` có `omitempty` trong `schema.Table`: bản ghi catalog cũ đọc như trước, chỉ cộng thêm) ở `CREATE TABLE` (mức bảng và mức cột) và `ALTER TABLE ADD CONSTRAINT`, hiện ở `pg_constraint`, `pg_index`, `information_schema`, `regclass`, và trong thông báo `23505`; `x::regclass::text` ra tên (`regclass_text.go`); `current_user` là tên đăng nhập; RowDescription có table OID và attnum cho cột lấy thẳng từ bảng (`colsrc.go`, `Column.TableOID/AttNum`; pgjdbc `getTableName`, ResultSet updatable chạy); `column_default` có `::character varying` / `::text`... và `true` chữ thường (`defaultcast.go`); thông báo `23503`/`23505` in đúng giá trị uuid/numeric/timestamp/bytea.
- **`oid`** làm kiểu cột (số nguyên, báo OID 26): changelog `BLOB` chạy. **Chưa** kiểm miền 0..2^32-1 khi ghi.

**Chưa làm / hạn chế.** Index biểu thức (`lower(col)`) vẫn `0A000`. Kiểu cột của view trong `information_schema.columns` vẫn `text`. `json`/`jsonb`/mảng vẫn không index được. Các số khác biệt còn lại của pgjdbc/SQLAlchemy (mảng nhị phân, `CREATE TYPE ... ENUM`, toán tử jsonb/mảng, `DECLARE CURSOR`) thuộc nhánh khác. `numeric` trong khoá ghép giải mã ra dạng chuẩn (`1.50` thành `1.5`).

**Kiểm chứng.** `internal/schema/indexkey_types_test.go` (property test thứ tự khoá numeric/timestamp/uuid/bytea so với so sánh của chính kiểu, tính tự phân định, đảo ngược; `-race`), `internal/sql/jmixfix_test.go` (`TestJmixIndexTypesProperty`: 3 seed, bảng có index so với bảng không index trên 8 cột, 80 truy vấn mỗi lượt, qua ghi/checkpoint/replica/restart; `TestJmixUniqueFK`, `TestJmixCompositeKeyNewTypes`, `TestJmixClientQueries`), `scripts/pgdiff/h_jmixfix.sql`, probe `jmix_*` (`cmd/claimcheck/probes_jmixfix.go`).

## 29. Đối tượng DDL cơ bản: kiểu enum, domain, schema và search_path, extension, COMMENT lưu thật (nhánh `ddlobjs`, 2026-10-30)

Trước nhánh này `CREATE TYPE`, `CREATE DOMAIN`, `CREATE SCHEMA`, `CREATE EXTENSION` bị từ chối (`0A000`) và `COMMENT ON` chỉ được nhận rồi bỏ. Giờ cả năm nhóm chạy, bền (WAL, checkpoint, replica) và so với PostgreSQL 16 thật bằng nhóm `h` của `pgdiff` (255 câu, 255 PASS).

**Lưu trữ: danh mục đối tượng `__stask_objs`.** Một bảng hệ thống ẩn (`core/objcat.go`, cùng mẫu `sqlview.go`: `Schema.System`, ghi như dòng thường nên đi qua WAL, checkpoint, replay, replica, MVCC; ẩn với client và CDC). Mỗi dòng là `(kind/name, kind, name, data)` với `kind` ∈ `schema`, `enum`, `domain`, `ext`, `comment`; `data` là JSON (hoặc nguyên văn lời chú thích). Không đổi định dạng log hay đĩa: chỉ thêm một bảng hệ thống mới, tạo lười lần ghi đầu. Thêm trường schema `Column.Domain` (`json:"domain,omitempty"`): dữ liệu cũ đọc như trước. Các lệnh áp dụng ngay, không theo giao dịch (như các DDL khác của STASK).

**(a) Enum.** `CREATE TYPE t AS ENUM (...)`, `DROP TYPE [IF EXISTS] [CASCADE]`, `ALTER TYPE .. ADD VALUE [IF NOT EXISTS] 'v' [BEFORE|AFTER 'x']`, `RENAME VALUE`, `RENAME TO`. Cột enum lưu nhãn (Type `string`, PG `enum:<tên>`). Thứ tự khai báo: giá trị đọc từ cột là datum `KText` mang theo OID kiểu (`Datum.I`) và vị trí sắp xếp (`Datum.F`, giống `pg_enum.enumsortorder`: 1, 2, 3, `BEFORE` lấy trung điểm), nên `ORDER BY`, `min/max`, `< > BETWEEN` và so với literal/tham số `$n` theo thứ tự khai báo; `m::text` bỏ dấu và sắp theo chữ như PG. Nhãn lạ: `22P02 invalid input value for enum`. OID kiểu là băm của tên (cột báo đúng OID trong `RowDescription` và `pg_type.oid`; khác OID của PG, vì PG cấp OID động), `pg_typeof`/`format_type` in tên kiểu. `DROP TYPE` đang dùng: `2BP01`; `CASCADE` bỏ cột phụ thuộc. `RENAME VALUE` viết lại các dòng đang giữ nhãn cũ (cột lưu chữ): thêm nhãn mới cùng vị trí, `UPDATE`, rồi bỏ nhãn cũ; **không nguyên tử** (mất điện giữa chừng để lại hai nhãn cùng vị trí, kiểu vẫn dùng được).

**(b) Domain.** `CREATE DOMAIN d AS base [DEFAULT e] [NOT NULL] [CONSTRAINT n] CHECK (... VALUE ...)`, `DROP DOMAIN`, `ALTER DOMAIN` (`SET/DROP DEFAULT`, `SET/DROP NOT NULL`, `ADD [CONSTRAINT n] CHECK`, `DROP CONSTRAINT [IF EXISTS]`, `RENAME TO`). Cột domain giữ kiểu nền (RowDescription báo OID kiểu nền, như PG), `Column.Domain` nhớ tên. Ràng buộc kiểm trong `checkRow` (mọi đường INSERT/UPDATE/COPY) và khi ép kiểu `x::d`: `23514 value for domain d violates check constraint "d_check"` (tên mặc định `d_check`, `d_check1`...), `23502 domain d does not allow null values`. DEFAULT của domain áp lúc tạo dòng (đọc từ danh mục, đổi sau vẫn ăn), không chép vào cột nên `\d+` không hiện. `ALTER DOMAIN SET NOT NULL`/`ADD CHECK` kiểm các dòng hiện có (`23502`/`23514`). `pg_type` (`typtype='d'`, `typnotnull`, `typbasetype`), `pg_constraint` (`contypid`), `information_schema.domains`, `\dD` đúng.

**(c) Schema và search_path.** `CREATE SCHEMA [IF NOT EXISTS]`, `DROP SCHEMA [IF EXISTS] [CASCADE]` (không CASCADE mà còn đối tượng: `2BP01`), `ALTER SCHEMA .. OWNER TO` (không làm gì). Bảng, view, sequence, enum, domain trong schema `s` lưu dưới tên `s.tên`; schema `public` giữ tên trần nên mọi thứ cũ vẫn ở `public`. `s.t` có thể dùng ở FROM, INSERT, UPDATE, DELETE, DDL, `nextval('s.q')`, `'s.t'::regclass`; `FROM s.t` có bí danh mặc định `t`. `search_path` là biến phiên thật: `SET [LOCAL]/RESET/SHOW` (mặc định `"$user", public` như PG), tìm quan hệ theo thứ tự, `CREATE` vào schema tồn tại đầu tiên của đường dẫn (`3F000` nếu không có), `current_schema()`, `current_schemas()`, `pg_table_is_visible/pg_type_is_visible` theo đường dẫn. Với đường dẫn mặc định không có tra cứu nào thêm (hành vi cũ không đổi). `pg_namespace`, `pg_class.relnamespace`, `pg_tables`, `pg_indexes`, `pg_views`, `information_schema.{schemata,tables,columns,sequences,views}` báo đúng schema.

**(d) Extension.** `CREATE EXTENSION [IF NOT EXISTS] x [WITH] [SCHEMA s] [VERSION v]`, `DROP EXTENSION [IF EXISTS]`. Nhận `pgcrypto`, `uuid-ossp`, `pg_trgm`, `btree_gin`, `btree_gist` (phiên bản của PG 16: 1.3, 1.1, 1.6, 1.3, 1.7); `citext` và `hstore` bị từ chối rõ (`0A000 .. is not supported: the type is not implemented`); tên khác `0A000 is not available`. `pg_extension` luôn có thêm `plpgsql 1.0` như CSDL PG mới (STASK không có PL/pgSQL; `CREATE EXTENSION plpgsql` báo đã có). Hàm: `pgcrypto`: `digest`, `hmac`, `gen_random_bytes`; `uuid-ossp`: `uuid_generate_v1/v1mc/v3/v5`, `uuid_nil`, `uuid_ns_*` (`uuid_generate_v4` vẫn dùng được không cần extension như trước); `pg_trgm`: `similarity`. Hàm của extension chưa cài: `42883`. Thêm các hàm lõi `encode/decode` (hex, base64, escape) và `sha224/256/384/512(bytea)`. **Chưa làm:** `crypt`, `gen_salt` (cần bcrypt: `golang.org/x/crypto` đang `// indirect` trong `go.mod`, dùng nó phải được người điều phối duyệt), toán tử `%`/`<->` của `pg_trgm`, kiểu `citext`/`hstore`; `CREATE INDEX .. gin_trgm_ops` vẫn được nhận khi chưa cài `pg_trgm` (PG sẽ báo lỗi).

**(e) COMMENT ON.** `COMMENT ON TABLE | COLUMN | INDEX | SEQUENCE | VIEW | TYPE | DOMAIN | SCHEMA | EXTENSION .. IS 'text' | NULL` lưu thật (`comment/<khoá>`); `''` hoặc `NULL` xoá. Đối tượng không tồn tại: `42P01`/`42703`/`3F000`/`42704`. Lời chú thích đi theo `RENAME TABLE`/`RENAME COLUMN`, mất khi `DROP TABLE/VIEW/SEQUENCE/INDEX/COLUMN/TYPE/SCHEMA` (tạo lại cùng tên không hiện lại chú thích cũ). `obj_description(oid, catalog)`, `col_description(oid, n)`, `pg_description` (kèm `public`: "standard public schema" và mô tả extension như PG), `psql \d+` cột Description. Các lớp đối tượng khác (`FUNCTION`, `CONSTRAINT`...) vẫn chỉ được nhận kèm NOTICE.

**psql 16 thật** (`/tmp/ddlobjs-psql`, `docker run postgres:16 psql` vào STASK so với PG): `\dn`, `\dn+`, `\dT`, `\dT+`, `\dD`, `\dx`, `\d+` (bảng có cột enum/domain và comment), `\d s.t`, `\dt s.*`, `\ds s.*` cho cùng bảng với PG, trừ cột Owner (`stask` thay `postgres`/`pg_database_owner`, ACL của `public`) và Storage `extended` của `text` (đã có từ trước). `\dT+` cần `CAST(x AS pg_catalog.text)`: parser nhận tiền tố `pg_catalog.` ở tên kiểu.

**Giới hạn đã biết (trung thực).**
- Tên được giải quyết **lúc parse**: trong một câu `simple query` nhiều lệnh (`SET search_path ..; SELECT ..`) cả lô parse trước khi chạy nên `SELECT` không thấy `SET` cùng lô; câu đã `Parse` (extended protocol) giữ nguyên tên đã giải quyết khi `search_path` đổi sau đó. Thân view/trigger lưu dạng chữ, parse lại không theo phiên: phải dùng tên có schema hoặc `public`.
- Cột ba phần `s.t.c` trong biểu thức chưa nhận (dùng `t.c`). Tên index chung một không gian (như trước); `DROP INDEX s.i` bỏ tiền tố.
- Chỉ mục phụ trên cột enum sắp theo chữ của nhãn; planner **không dùng** chúng cho cột enum (điều kiện, `IN`, `ORDER BY` quét tuần tự rồi sắp đúng thứ tự khai báo, kiểm bởi `TestDDLObjsEnumIndex`), nên chậm hơn PG ở đó nhưng đúng. Khoá chính là cột enum: tra bằng `=` đúng, nhưng thứ tự khoá vẫn theo chữ (chưa xử lý). Mảng enum/domain (`mood[]`) thành `text[]`. Domain trên domain/enum bị từ chối.
- `pg_typeof(cột_domain)` trả tên kiểu nền (PG trả tên domain). `ALTER TABLE ADD COLUMN c dom` không điền DEFAULT của domain cho dòng cũ.
- Registry kiểu (`ddlobjs_types.go`) là toàn tiến trình, nạp lại từ engine mỗi lần câu đụng bảng có cột enum/domain; hai engine trong một tiến trình dùng chung tên kiểu khác nhãn sẽ giẫm nhau (chỉ có thể xảy ra trong test).
- `ALTER SCHEMA .. RENAME TO`, `ALTER EXTENSION`, `CREATE SCHEMA` kèm phần tử, `ALTER COLUMN TYPE` sang enum/domain: `0A000`.
- OID của enum/domain/schema/extension là băm của tên, không trùng OID của PG; công cụ so sánh OID (pgdiff) phải ép `::text`.

**Kiểm chứng.** `internal/sql/ddlobjs_test.go` (enum, domain, schema, search_path, extension, comment, restart có checkpoint), `internal/repl/ddlobjs_repl_test.go` (replica theo log và qua restart), `internal/pgwire/ddlobjs_wire_test.go` (pgx: tham số vào cột enum/domain, OID trong RowDescription, search_path theo kết nối, comment, `digest`), `-race` sạch cho các test mới; `scripts/pgdiff/h_ddlobjs.sql`; `cmd/claimcheck/probes_ddlobjs.go` (47 probe `do_*`).

## 30. Tương thích Superset/SQLAlchemy: GROUP BY biểu thức, numeric, to_char, regclass, EXISTS (nhánh `supfix`, 2026-10-30)

Nguồn: nhóm pgdiff `g` (docs/14 Phụ lục E) và `h_supfix.sql` (Phụ lục F). Mọi hành vi dưới đây đối chiếu với PostgreSQL 16 thật (pgdiff nhóm `g` và `h`, probe `sq_*`/`sf_*` của `claimcheck`). Logic nằm ở file mới `internal/sql/supfix_*.go`; chỗ nối vào mã có sẵn chỉ một vài dòng.

**GROUP BY biểu thức.** `exprKey` (so khớp biểu thức giữa SELECT/HAVING/ORDER BY và GROUP BY) trước đây chỉ biết cột, hằng, toán tử, hàm và `CAST`; `CASE`, `IN`, `BETWEEN`, `LIKE`, `IS NULL`, hàng lồng bị so theo con trỏ nên lặp lại biểu thức báo `42803`. Nay mọi nút trên đều có khoá cấu trúc (`supfix_keys.go`), nên `SELECT CASE WHEN .. END AS b, count(*) .. GROUP BY CASE WHEN .. END`, `date_trunc(..)`, `extract(..)`, `to_char(..)` lồng nhau chạy như PG. Một biểu thức khác ở SELECT và GROUP BY vẫn bị `42803`.

**numeric không đi qua float64.** Ba chỗ:
1. Kiểu tĩnh của kết quả ở executor tổng quát (`staticKind`, `supfix_types.go`): `numeric (*,+,-,/) int` là `numeric` (trước: `float8`, nên `SUM(qty*unit_price)` qua JOIN ra OID 701 ở giao thức mở rộng nhưng `22.00` ở giao thức đơn: cùng một câu hai kết quả khác nhau; nay cả hai ra `22.00`, OID 1700). `extract(epoch ..)*1000` là `numeric` với 6 chữ số thập phân.
2. `stddev/variance/var_pop/..` trên `int` và `numeric` tính bằng số thập phân đúng như `numeric_stddev_internal` của PG (tổng và tổng bình phương chính xác, chia theo `select_div_scale`, căn bậc hai làm tròn theo thang) và trả `numeric` (`var_pop(1,2,2,3,5)` = `1.8400000000000000`); trên `float8` dùng cộng dồn Youngs-Cramer của `float8_accum` (khớp từng chữ số cuối).
3. `sum(bigint)` là `numeric`, `sum(int2/int4)` là `bigint` (như PG), cả khi là cửa sổ (`SUM(c) OVER ()` với `c = count(*)`). Giá trị vẫn `int64` bên trong; cột công bố `numeric` nên giao thức in `5000` và OID 1700.

Còn lại: các phép tính giữa `float8` và `numeric` vẫn ra `float8` (như PG); `SUM(bigint)` tràn `int64` vẫn tràn (chưa sửa).

**Hàm tổng hợp hai đối số** (`supfix_aggs_more.go`): `corr`, `covar_pop/samp`, `regr_count/avgx/avgy/sxx/syy/sxy/slope/intercept/r2`, cộng dồn như `float8_regr_accum`; so với PG khớp từng chữ số trên 750 dòng/nhóm. `percentile_cont/disc WITHIN GROUP` đã có. `json_agg`: chưa làm (nhánh khác).

**`to_char(timestamp, mẫu)` đầy đủ bảng mẫu của PG** (`supfix_tochar.go`): `IW IYYY IYY IY I ID IDDD W WW Q CC J DDD`, `Y,YYY YYYY YYY YY Y`, `AM am A.M. a.m. PM ..`, `BC AD`, `RM rm`, `TZ tz TZH TZM OF`, `FF1..FF6 MS US SSSS`, hậu tố `TH th`, tiền tố `FM`, chữ thường của mẫu số (`yyyy-mm-dd hh24:mi:ss`), chuỗi trong nháy kép và `\`. `to_char(created, 'YYYY-"W"IW')` ra `2026-W02` (trước: `2026-WIW`, một nhóm duy nhất). Múi giờ luôn UTC (phiên chưa có `TimeZone` riêng).

**Catalog.**
- `x::regclass::text`, `to_regclass(x)::text`, `::regclass::varchar` ra tên quan hệ (có nháy kép khi cần, `-` cho OID 0); `x::regclass` đứng một mình vẫn là số OID (kiểu `regclass` chưa là Kind riêng).
- Kiểu cột của `pg_catalog` và `information_schema` đúng PG 16 (sinh bằng cách so `select * from <bảng>` qua `\gdesc` với PG thật, `zz_supfix_cattypes.go`): `name` (19), `"char"` (18), `oid` (26), `int2` (21), `int4`, `real`, `xid`, `varchar(3)`, `pg_node_tree`, `int2vector`, `oidvector`; giá trị không đổi. Nhị phân của `oid`/`xid`/`"char"` có (`supfixBinary`). Hệ quả cho client: Go/pgx đọc `pg_class.oid` phải vào `uint32` (như với PG thật; trước đây vào `int64` được).
- `current_database()`, `current_user`, `current_schema()`, `pg_get_userbyid()`, `pg_encoding_to_char()` kiểu `name`.
- `pg_constraint.confmatchtype` là `s` cho khoá ngoại. Năm domain của `information_schema` có trong `pg_type` (`typtype='d'`), vì `get_columns` của SQLAlchemy 2.0 luôn hỏi domain.
- `reltuples`/`relpages` lấy từ số dòng sống (`LiveRows`) và `pg_relation_size`/`pg_total_relation_size` ước theo dòng x cột (8 kB/trang, bảng rỗng 0): **số gần đúng, không phải thống kê** (PG cho `reltuples = -1` tới khi `ANALYZE`; STASK không có ANALYZE thật).
- `pg_get_viewdef(view, pretty)` và `pg_views.definition` in theo bố cục PG (`SELECT a,\n    b\n   FROM t\n  WHERE (c = 'x'::text);`) cho view đơn giản (cột/biểu thức có bí danh, một bảng hoặc `JOIN`/`LEFT JOIN` bảng, WHERE gồm so sánh/AND/OR/NOT/IN/IS NULL/LIKE/BETWEEN, GROUP BY, ORDER BY, LIMIT). Việc `::numeric` thêm vào hằng số nguyên so với cột numeric và dấu ngoặc quanh JOIN của dạng không pretty **chưa làm**; construct khác giữ nguyên văn truy vấn đã lưu.
- `set_config` trả giá trị sau khi đặt (`5s`), `SET search_path TO a, b` giữ `a, b`, `SHOW max_connections` = `100`, `pg_sleep(giây)` (ngắt được bởi huỷ/timeout).

**Cú pháp.** `WITH x AS [NOT] MATERIALIZED (..)`; nhãn cột có `""` bên trong (`AS "a ""b"""`); `CREATE INDEX` trên `timestamp`/`timestamptz` (khoá index là micro giây có dấu, như `time`).

**Lưu ý đã biết.** `WITH c(y) AS (..), d AS (..) SELECT d.*` in thừa cột của `c` (phát hiện, chưa sửa).

**EXPLAIN có ước lượng thô.** Mỗi dòng nút kết thúc bằng `(cost=a..b rows=n width=w)` tính từ `LiveRows` và hằng số giống PG (seq scan = trang + 0,01 x dòng; lọc = 1/3 dòng; HashAggregate = min(200, dòng); sort = n log n ...), dòng cuối ghi `cost= and rows= are rough estimates from live row counts, not planner statistics`. Mục đích: nút "ước tính chi phí" của SQL Lab (khớp `cost=([\d.]+)\.\.([\d.]+)` ở dòng đầu) trả số. **Không phải thống kê của bộ lập kế hoạch**; đừng so với PG. `EXPLAIN (FORMAT JSON)` vẫn trả `text`.

**EXISTS/IN tương quan chạy băm (`supfix_semijoin.go`, `supfix_scan.go`).** `semijoin.go` (nhánh `sqlgaps`) đã băm khi tương quan là AND của các phép `=` và truy vấn con là SELECT phẳng; truy vấn `reserved_sold_have_items` của chế độ bóng thuộc dạng đó, nên trên `main` nó chạy 1,5 s (260k SIM, 150k dòng) chứ không còn 52 đến 104 s: con số đó thuộc binary chưa có `sqlgaps` (binary tại `61f6755^1` chạy quá 170 s, hết hạn đo). Phần còn thiếu, nay có: (1) vị từ phụ nhắc tới dòng ngoài (`.. AND i.price > s.price`): băm theo khoá `=`, mỗi dòng ngoài chỉ thử các dòng cùng khoá (`SubPlan: Hash Semi Join .. Join Filter` trong EXPLAIN); (2) `GROUP BY`/`HAVING` trong truy vấn con (nhóm theo nhóm của chính nó + khoá tương quan; `HAVING` không có `GROUP BY` nhóm theo khoá và khoá không có dòng nào lấy giá trị của `HAVING` trên tập rỗng, như PG); (3) `DISTINCT`, `ORDER BY`, `LIMIT n >= 1` trong `EXISTS`. `HAVING` **không** bị bỏ qua (nghi ngờ của `sqlgaps` sai: so với PG ở cả hai đường đều đúng); chỉ là chạy lồng nên chậm. Kết quả bằng bản lồng trên dữ liệu ngẫu nhiên có NULL (`TestSupfixSemiJoinForms`, 30 truy vấn). Ngoài ra bảng được nạp vào bộ nhớ bằng quét song song (cùng bộ trợ thủ co giãn của `lanes.go`, giữ đúng thứ tự quét tuần tự; bảng < 20.000 dòng hoặc có ghi chưa commit của chính phiên vẫn quét tuần tự), `Dec.String` có đường nhanh cho hệ số vừa 64 bit và tra khoá băm không cấp phát: truy vấn bóng từ 1,4 s xuống ~0,5 s (VM tải 3 đến 10).

## DDL trong giao dịch (nhánh `txddl` 2026-10-30, hoàn thiện ở nhánh `txddl2`)

Trước đây mọi DDL áp dụng ngay và không hoàn tác (`BEGIN; CREATE TABLE ..; ROLLBACK` để lại bảng). Liquibase chạy mỗi changeSet trong một giao dịch nên đây là lỗ hổng. Nay trong `BEGIN .. COMMIT/ROLLBACK`, `ROLLBACK TO SAVEPOINT` và khi phiên đóng / crash, các câu DDL dưới đây hoàn tác được. Hai cơ chế:

**A. Áp dụng ngay + undo** (phần còn lại của giao dịch thấy kết quả ngay; một bước *undo* được ghi vào nhật ký **trước** câu lệnh):

| Câu lệnh | Undo |
|---|---|
| `CREATE TABLE`, `CREATE TABLE AS`, `SELECT INTO` (không TEMP) | `DROP TABLE` đúng bảng đó (nhận diện bằng `RelID` đặt trước, xem "cửa sổ crash") |
| `CREATE INDEX` (btree) | `DROP INDEX` các index mới |
| `ALTER TABLE .. ADD COLUMN` | `DROP COLUMN` các cột mới |
| `ADD CONSTRAINT .. UNIQUE / FOREIGN KEY`, `ADD CHECK` | bỏ các ràng buộc/index mới sinh |
| `RENAME COLUMN`, `RENAME TO` (bảng) | đổi tên lại |
| `ALTER COLUMN TYPE / SET DEFAULT / DROP DEFAULT / SET NOT NULL / DROP NOT NULL` | đặt lại nguyên định nghĩa cột cũ (engine chỉ thay schema, không ghi lại dòng, nên đảo lại chính xác) |
| `CREATE/DROP VIEW`, `CREATE OR REPLACE VIEW` | ảnh trước của view (và comment) |
| `CREATE/DROP/ALTER SEQUENCE` | ảnh trước của sequence (định nghĩa + giá trị; `nextval` vẫn không giao dịch như PG) |
| `CREATE/DROP TRIGGER` | ảnh trước của trigger |
| `CREATE/DROP TYPE` (enum), `CREATE/DROP DOMAIN`, `ALTER DOMAIN` (trừ RENAME), `CREATE/DROP SCHEMA`, `CREATE/DROP EXTENSION`, `COMMENT ON` | ảnh trước của các hàng danh mục đối tượng liên quan |
| `TRUNCATE` | chạy như *xoá mọi dòng bằng chính giao dịch* (O(số dòng), trigger DELETE không bắn như TRUNCATE; `RESTART IDENTITY` có undo cho sequence); ROLLBACK huỷ các xoá. `TRUNCATE` ngoài giao dịch vẫn O(1) |

**B. Ẩn hoặc hoãn** (câu phá dữ liệu):

* `DROP TABLE` và `ALTER TABLE .. DROP COLUMN` **ẩn ngay** bằng cách đổi tên đối tượng sang tên riêng `__txd_<id>_<n>_..` (bảng: kèm sequence sở hữu; index của bảng bị xoá và dựng lại khi hoàn tác vì tên index là toàn cục). Giao dịch thấy đối tượng *đã mất* (`SELECT` báo `42P01`, `DROP` lần hai báo lỗi, `CREATE TABLE` cùng tên chạy được: `DROP TABLE a; CREATE TABLE a (..)` trong một giao dịch hoạt động). Undo: đổi tên lại; tại COMMIT bước *redo* xoá thật đối tượng ẩn. Không ẩn được (rơi về *hoãn*) khi: bảng có trigger, có khoá ngoại từ bảng khác, có view phụ thuộc (CASCADE), hoặc **chính giao dịch này đã ghi dòng vào bảng** (đổi tên sẽ làm COMMIT xung đột; xem giới hạn 3), hoặc cột có CHECK, hoặc `DROP TABLE a, b` nhiều bảng.
* *Hoãn*: kiểm tra ngay (tồn tại, phụ thuộc view/FK `2BP01`, không xoá cột khoá, trigram/search/time-series), ghi bước redo, đối tượng vẫn thấy được đến COMMIT. Áp dụng cho `DROP INDEX`, `DROP CONSTRAINT` và các trường hợp "không ẩn được" ở trên. Hệ quả: trong trường hợp hoãn, `DROP TABLE a; CREATE TABLE a` báo "đã tồn tại".

**Savepoint.** Mỗi `SAVEPOINT` nhớ vị trí nhật ký; `ROLLBACK TO` chạy ngược các bước undo sau vị trí đó (sau khi `Tx.RollbackTo` bỏ các ghi dữ liệu), cắt các bước redo, và ghi lại nhật ký; `RELEASE` giữ nguyên. Nếu một bước undo lỗi thì giao dịch chuyển sang trạng thái lỗi và các bước còn lại được thử lại ở `ROLLBACK` cuối.

**Thiết kế (không đổi định dạng đĩa/log).** Nhật ký là hai hàng của bảng danh mục đối tượng `__stask_objs` (kind `ddltx`, JSON, đi theo WAL, checkpoint, replica như mọi hàng): `o-<id>` chứa các bước undo, ghi **ngoài** giao dịch trước mỗi câu DDL; `c-<id>` chứa các bước redo và được ghi **bên trong** giao dịch của người dùng ngay trước `tx.Commit()`, nên nằm cùng một bản ghi commit với các dòng dữ liệu (nguyên tử). Sau commit, redo chạy rồi cả hai hàng bị xoá. Khôi phục (một lần mỗi engine, ở phiên đầu tiên; replica không làm): có `o-` và `c-` → giao dịch đã commit, chạy lại redo; chỉ có `o-` → chưa commit, chạy undo ngược. Mọi bước idempotent. Một thay đổi nhỏ ngoài nhật ký: một giao dịch đọc view mà **chính nó** đã tạo/đổi/xoá theo trạng thái mới nhất (view là đối tượng danh mục áp dụng ngay, không theo snapshot; view do phiên khác đổi vẫn theo snapshot, `TestSQLViewSnapshot`).

**Redo hỏng khi COMMIT (nhiệm vụ 3).** Quy tắc: *đã commit thì phải thắng*. (1) Mọi kiểm tra được làm ở lúc câu lệnh chạy; với `DROP TABLE`/`DROP COLUMN` đã ẩn thì đối tượng không còn tên nên không thể sinh phụ thuộc mới, redo không thể hỏng vì phụ thuộc. (2) Với DROP hoãn, redo chạy `CASCADE`: view hay khoá ngoại mà phiên khác tạo trong lúc chờ bị xoá theo, và `COMMIT` trả `Notice` nêu tên ("drop cascades to view ..."). (3) Nếu redo vẫn hỏng (lỗi I/O), `COMMIT` **không** im lặng: dữ liệu đã bền nên không thể trả lỗi "giao dịch chưa commit", mà trả `Notice` `WARNING: the transaction committed but its deferred DDL failed (...)`, ghi log, **giữ** hai hàng nhật ký, thử lại 3 lần ngay và ở **mọi** lần khởi động cho tới khi xong (trước đây thử đúng một lần rồi xoá nhật ký).

**Cửa sổ crash của `CREATE TABLE` (nhiệm vụ 5).** Bước undo được ghi trước khi bảng có ID. Nay bảng được tạo với `RelID` (OID công bố) là một số ngẫu nhiên ≥ `0x40000000` chọn trước và ghi trong bước undo; undo chỉ xoá bảng mang đúng `RelID` đó. Vì vậy: crash giữa lúc ghi nhật ký và lúc `CREATE`, hay tên bị phiên khác chiếm, không bao giờ xoá nhầm bảng của người khác. Hệ quả: OID của bảng tạo trong giao dịch là số lớn (hợp lệ, ổn định, giữ qua TRUNCATE).

**Giới hạn, nói thẳng.**
1. Phiên khác **thấy** đối tượng mới của giao dịch chưa commit, thấy bảng/cột đã ẩn là *đã mất* (PG thì không cả hai), và nếu giao dịch rollback thì chúng quay lại/biến mất; dữ liệu phiên khác ghi vào đối tượng đó bị mất theo hoặc gây lỗi. Một ghi vào bảng bị ẩn từ phiên khác báo `42P01`.
2. Không hoàn tác được (áp dụng ngay như trước): `ALTER TABLE .. ADD PRIMARY KEY`, `ALTER TYPE` (mọi dạng: RENAME và RENAME VALUE ghi lại dòng/cột, ADD VALUE không tách riêng), mọi `DROP .. CASCADE` của type/domain/schema (xoá cột, bảng), `ALTER SCHEMA`, `GRANT/REVOKE`/role, publication/replica identity (CDC), chỉ mục trigram (`CREATE/DROP`), bảng TEMP, view vật chất hoá, `ALTER TABLE .. SET (..)`. Với bảng có trigger, DROP chỉ hoãn.
3. **Giới hạn có sẵn của engine, chưa sửa (đổi đường commit):** `COMMIT` báo "could not serialize access" nếu giao dịch đã ghi dòng vào bảng rồi *đổi schema bảng đó* (`ALTER`, kể cả khi hoàn tác nó bằng `ROLLBACK TO`): dòng đã mã hoá theo schema cũ. Vì thế `INSERT ..; ALTER TABLE .. ADD COLUMN ..; COMMIT` thất bại cả khi không có savepoint; ghi sau ALTER thì được. Ẩn `DROP TABLE` cũng đổi schema nên không dùng khi đã ghi (xem trên).
4. `TRUNCATE` trong giao dịch: chi phí O(số dòng); khoá ngoại tự tham chiếu có thể báo lỗi vi phạm theo thứ tự xoá; trigger DELETE không bắn.
5. Hoàn tác `DROP TABLE` đã ẩn dựng lại index từ đầu (O(số dòng) khi ROLLBACK).
6. Hoàn tác `DROP NOT NULL` kiểm lại các dòng và có thể thất bại nếu phiên khác đã ghi NULL trong lúc chờ (giới hạn 1).
7. Giao dịch ngầm (autocommit) không đổi: một câu DDL vẫn một bản ghi log.

**Bảng tạm và log.** Dòng của bảng tạm vẫn ghi vào log bền và đi tới replica; **không làm được bằng thay đổi nhỏ**. Bằng chứng: `core.Version` (`engine.go:~113`) giữ `Loc wal.Loc` và `ValOff/ValLen`; chỉ mục chính không chứa giá trị, `Engine.readValue`/`loadVersion` đọc giá trị **từ log** (`e.log.ReadOpMeta`); log vừa là WAL vừa là kho giá trị (docs/01, docs/03). Một bảng "không ghi log" cần kho giá trị thứ hai trong RAM, đường đọc/GC/checkpoint/replica/xoá-bảng riêng, tức đổi đường commit và định dạng checkpoint: quyết định thiết kế của người điều phối. Giảm thiểu: bảng tạm là `System` (ẩn khỏi CDC/publication/catalog phiên khác), bị xoá khi phiên đóng và quét khi khởi động.

**Kiểm chứng.** `internal/sql/ddltx_test.go` và `ddltx2_test.go`: (i) bộ **oracle**: cùng một kịch bản 35 câu DDL (ràng buộc, đổi tên, đổi kiểu, view, sequence, trigger, type, domain, schema, comment, truncate, drop+create cùng tên) chạy autocommit trên engine A và trong `BEGIN..COMMIT` trên engine B phải cho **cùng** trạng thái (`dbState`: catalog + mọi dòng); `ROLLBACK`, **crash trước commit** và **crash sau commit trước redo** (mỗi cái có/không checkpoint, mở lại engine) phải về trạng thái trước / trạng thái oracle, không để lại nhật ký hay đối tượng ẩn; (ii) savepoint: `ROLLBACK TO` hai lần, `RELEASE`, lồng nhau, lỗi rồi `ROLLBACK TO`, crash sau `ROLLBACK TO`; (iii) `DROP`+`CREATE` cùng tên: commit, rollback, crash trước/sau commit; (iv) redo hỏng (tiêm lỗi) → `COMMIT` có WARNING, nhật ký giữ, khởi động lại hoàn tất; DROP hoãn thắng view của phiên khác; (v) nhận diện bảng bằng `RelID`; (vi) 8 phiên song song (`-race -count=3`). 29 probe `claimcheck` `tx_*`/`tx2_*` (`cmd/claimcheck/probes_txddl*.go`). Liquibase thật: `accept/clients/liquibase/txddl.sh` (changeSet 3 lệnh DDL rồi lỗi: PG và STASK để lại đúng cùng trạng thái, `diff` 0 dòng; trên `main` cũ: 13 dòng khác vì `DROP TABLE T1; CREATE TABLE T1` thất bại).

## Collation văn bản (nhánh `collation`, 2026-10-30)

`staskd -collation <tên>` (mặc định `C` = so byte, như trước). Tên nhận: `C`, `POSIX`, `C.UTF-8`, `en_US.utf8`, `vi_VN.utf8`, `und-x-icu`, hoặc locale CLDR `xx_YY[.utf8]`/`xx-x-icu`. `COLLATE "tên"` theo biểu thức đổi collation của ORDER BY item, `< <= > >=` và BETWEEN; tên lạ báo `42704`. Chỉ **thứ tự** đổi; `=`, IN, GROUP BY, DISTINCT, UNIQUE/PK vẫn so byte. Chỉ mục xếp byte nên khi collation khác C, `ORDER BY text` và khoảng `< > BETWEEN` trên text không dùng chỉ mục (`=`/IN vẫn dùng; `COLLATE "C"` vẫn dùng). `SHOW lc_collate`, `pg_database.datcollate/datctype`, `pg_collation`, `current_setting` báo cài đặt. `COLLATE` khai báo trên cột/chỉ mục trong DDL vẫn nhận và bỏ qua. Độ khớp với PostgreSQL `en_US.utf8` (glibc 2.41) và hệ quả với `ORDER BY ... LIMIT`: `docs/14` Phụ lục W.
