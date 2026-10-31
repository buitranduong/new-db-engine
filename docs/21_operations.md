# 21 — Vận hành: chuyển đổi không dừng, HA, sao lưu, giám sát, bảo mật

Tài liệu này là sổ tay vận hành của nhánh `opsha` (2026-10-31). Mọi mục dưới đây có test kèm; chỗ nào chưa kiểm hay chưa làm thì ghi rõ ở "Giới hạn". Bằng chứng chi tiết và lệnh tái lập: `docs/14_plan_target_actual.md` Phụ lục OPS.

| § | Nội dung |
|---|---|
| 1 | Chọn đường HA: replica bất đồng bộ + promote hay cụm Raft (quyết định kèm bằng chứng) |
| 2 | Chuyển DB đang chạy (kho, CRM) sang STASK không dừng: bóng, chuyển đổi, quay lại |
| 3 | Cụm Raft 3 nút trong production: cờ, DSN client, mất leader, split-brain, khởi động lại cuốn chiếu, nâng cấp |
| 4 | Sao lưu, kiểm khôi phục tự động, khôi phục (`staskctl`) |
| 5 | Giám sát: `/metrics`, `/v1/health`, `/v1/ready`, ngưỡng cảnh báo |
| 6 | Nhật ký (cấp độ, JSON) |
| 7 | Bảo mật: rà soát và các cờ |
| 8 | Quy trình xử lý sự cố |
| 9 | Giới hạn và việc còn lại |

---

## 1. Chọn đường HA

Hai ứng viên có sẵn trong mã: (i) `internal/repl` (replica bất đồng bộ, promote bằng cách mở thư mục replica làm leader), (ii) `internal/raft` + `internal/cluster` (xác nhận ghi theo đa số, bầu leader tự động).

| Tiêu chí | (i) replica bất đồng bộ + promote | (ii) cụm Raft 3 nút |
|---|---|---|
| Ghi đã trả lời "xong" có thể mất khi leader chết | **Có** (leader trả lời trước khi replica nhận) | Không (trả lời sau khi đa số đã fsync) |
| Chọn leader mới | Thủ công, hoặc cần thêm một bộ điều phối ngoài (chưa có trong mã) | Tự động, trung vị 1,72 s (đo trên tiến trình thật, docs/06 §5.2) |
| Chống hai leader (split-brain) | Cần hàng rào ngoài (STONITH, VIP); không có trong mã | Có trong giao thức: term, check-quorum, lease |
| Chi phí độ trễ commit | ~0 | 3 đến 4 lần p50 (docs/06 §5.2) |
| Sức ghi | như một nút | như một nút (mọi nút áp mọi ghi) |
| Thay nút hỏng | Dựng replica mới từ base backup | **Chưa có đổi thành viên** (§9) |

**Bằng chứng ghi mất** (`TestAsyncReplicaLosesAckedWrites`, `internal/cluster/async_test.go`: leader + replica thật, 8 client ghi qua HTTP, `kill -9` leader, mở thư mục replica làm leader, đếm ghi đã xác nhận mà thiếu):

| Lượt chạy | Ghi đã xác nhận | Mất sau promote |
|---|---|---|
| 4 lượt (lần 1) | 1.667 | **6** (một lượt mất 6, ba lượt mất 0) |
| 12 lượt (lần 2) | 5.010 | **3** |

Mất không xảy ra mọi lượt (cửa sổ giữa lúc leader trả lời và lúc replica nhận chỉ vài ms) nhưng xảy ra, và số mất thay đổi từng lần; đó là tính chất của nhân bản bất đồng bộ, không phải lỗi.

So với cụm Raft: `TestProcChaos` 60 vòng sự cố, 88.981 ghi xác nhận, 0 mất (docs/06 §5.2), và lượt chạy mới trong nhánh này ở Phụ lục OPS.

**Quyết định (đề xuất cho `docs/PROCESS.md`, người điều phối ghi):** đường HA production là **cụm Raft 3 nút**. Replica bất đồng bộ (`-replica-of`) giữ lại cho đọc phân tích và dự phòng nguội, không dùng để tự động chuyển đổi dự phòng khi dữ liệu không được phép mất. D18 (đóng băng Raft) được bỏ cho nhiệm vụ này: việc nối vào engine đã có từ P6.2; phần mới của nhánh này là báo vai trò trên giao thức PG (để driver tìm leader), sao lưu cụm, và bộ test HA qua PG. Đổi thành viên vẫn **chưa** làm (§9).

---

## 2. Chuyển DB đang chạy sang STASK không dừng (bóng, chuyển đổi, quay lại)

Công cụ: `stashadow` (`cmd/stashadow`, `internal/shadow`). PostgreSQL vẫn là nơi ghi cho tới lúc chuyển; STASK nhận bản sao liên tục qua nhân bản logic (`pgoutput`), mỗi giao dịch nguồn là đúng một giao dịch STASK, LSN đã áp nằm trong cùng giao dịch.

### 2.1 Chuẩn bị

1. PostgreSQL nguồn: `wal_level=logical`, `max_replication_slots`, `max_wal_senders` đủ; một role có `REPLICATION` (hoặc superuser) cho công cụ.
2. **Schema trên STASK do người vận hành dựng** (công cụ chỉ sao chép cột và dòng): `pg_dump --schema-only` rồi nạp vào STASK (nhánh `crmdata` đã làm như vậy với CRM), kể cả index, default, `serial`/`identity`, khoá ngoại. `-create-tables` chỉ dựng cột + khoá chính, dùng cho thử nghiệm.
3. Mọi bảng cần khoá chính, hoặc `REPLICA IDENTITY FULL` (`-set-replica-identity` làm hộ).
4. Kiểu cột hỗ trợ: bool, int2/4/8, text, varchar, char, numeric, float4/8, uuid, bytea, timestamp, timestamptz, date, json, jsonb. Kiểu khác: công cụ dừng và nói cột nào.

### 2.2 Chạy bóng liên tục

```sh
stashadow run -source postgres://repl@pg:5432/crm -stask postgres://app@stask:5432/stask \
              -tables a,b,c -metrics-addr 127.0.0.1:9187 -txlog /var/log/stashadow.tx
```

- Lần đầu: ảnh chụp bằng slot xuất snapshot (`COPY` CSV trong snapshot đó), sau đó theo dõi thay đổi. Chạy lại cùng lệnh: tiếp tục từ `shadow_ctl.applied_lsn`.
- **Khởi động lại hai phía** (`TestShadowBothSidesRestartLive`): `kill -9` staskd trong lúc công cụ chạy, rồi restart PostgreSQL; công cụ tự nối lại (backoff 1 đến 10 s) và tiếp tục từ LSN lưu trong STASK, dữ liệu giống nhau từng dòng. Công cụ chết/khởi động lại: `TestShadowEndToEnd` (kill -9 staskd và công cụ) và `scripts/shadow/chaos.sh`.
- **Báo độ trễ**: `GET /metrics` của `-metrics-addr` (`stashadow_lag_bytes`, `stashadow_lag_seconds`, `stashadow_streaming`, `stashadow_reconnects_total`, `stashadow_schema_changes_total`, `stashadow_transactions_total`, `stashadow_rows_total`, `stashadow_anomalies_total`); `stashadow status` (LSN nguồn / đã ack / đã áp). `lag_bytes` về 0 khi rỗi và đã bắt kịp (WAL của bảng ngoài danh sách không tính là trễ). Công cụ phải **luôn chạy**: slot giữ WAL trên nguồn, công cụ chết lâu thì đĩa PostgreSQL đầy (đặt cảnh báo trên `pg_replication_slots`, §5.3).

### 2.3 ALTER TABLE trên nguồn trong lúc chạy

`-schema-changes apply` (mặc định). Công cụ so hình dạng bảng trong thông điệp Relation của `pgoutput` với hình dạng đã áp (lưu trong bảng `shadow_schema` của STASK) rồi áp phần chênh lệch lên STASK **trước** thay đổi đầu tiên theo hình dạng mới:

| Trên PostgreSQL | Trên STASK |
|---|---|
| `ADD COLUMN c` | `ADD COLUMN c`; nếu có `DEFAULT` ở nguồn thì điền các dòng cũ từ nguồn (`backfill`) |
| `DROP COLUMN c` | `DROP COLUMN c` |
| `RENAME COLUMN a TO b` | `RENAME COLUMN` (phân biệt với drop+add bằng `attnum` của nguồn) |
| `ALTER COLUMN c TYPE ...` (cả đổi độ dài, độ chính xác) | `ALTER COLUMN c TYPE ...` |

Đã kiểm (`TestSchemaChangeWhileStreaming`): thêm cột có default, thêm cột thường, đổi tên, nới `varchar`, `int` lên `bigint`, rồi **tắt công cụ**, drop và đổi tên tiếp, bật lại; so sánh cuối khớp. Các thao tác lặp lại an toàn sau khi chết giữa chừng. `-schema-changes stop`: dừng và in câu DDL cần chạy.

Không mang qua: index, default, ràng buộc, bảng mới (thêm bảng: dừng công cụ, thêm vào `-tables` và `ALTER PUBLICATION ... ADD TABLE`, dựng bảng STASK, chạy với `-reset`), đổi khoá chính (công cụ dừng và nói rõ). Ghi chú về `backfill`: chạy song song với luồng, nên một dòng vừa đổi ở nguồn có thể tạm mang giá trị mới hơn rồi bị luồng ghi đè bằng giá trị cũ hơn trước khi hội tụ; khi hết trễ thì khớp.

### 2.4 Kiểm tra: `stashadow verify`

```sh
stashadow verify -a postgres://pg... -b postgres://stask... [-tables x,y] [-retries 5] [-json out.json]
```

Kỹ thuật của `accept/clients/crm/data/parity.py` (nhánh `crmdata`): mỗi phía tự tính trong engine của mình số dòng và `md5` của danh sách đã sắp của `md5(concat_ws('|', cột::text hoặc '<N>'))`; chỉ đếm và băm đi qua mạng, **không có dòng nào**. Không phụ thuộc thứ tự dòng. Thoát 1 nếu có bảng lệch. Chạy được cả hai hướng (`-a STASK -b PostgreSQL` sau khi chuyển). Với nguồn đang ghi dùng `-retries`. `stashadow compare` (đã có) cho thêm khối khoá lệch, mẫu ngẫu nhiên và bộ truy vấn. Đã kiểm: `TestCutoverFailbackRehearsal`, `TestSchemaChangeWhileStreaming`, và phát hiện sửa lén một ô (`TestShadowEndToEnd`).

### 2.5 Quy trình chuyển đổi (cutover) và quay lại (failback)

Bài diễn tập tự động: `TestCutoverFailbackRehearsal` (PostgreSQL 16 trong docker + staskd thật, ghi liên tục trong lúc chạy). Mỗi bước dưới đây là một bước của bài diễn tập.

**A. Trước giờ chuyển**
1. `stashadow status` và `/metrics`: `lag_seconds` thấp ổn định, `reconnects` không tăng, `anomalies = 0`.
2. `stashadow verify -retries 5` khớp. Cho ứng dụng chạy thử đọc trên STASK (bóng).
3. Role ứng dụng trên STASK: tạo bằng `CREATE ROLE app LOGIN PASSWORD ...` và `GRANT` đúng quyền (cần để đóng băng STASK khi quay lại, bước D).

**B. Đóng băng nguồn, xả, kiểm** (cửa sổ không ghi: giây, không phải giờ)
4. Đóng băng PostgreSQL bằng cách **thu quyền ghi của role ứng dụng** và ngắt phiên của nó:
   ```sql
   REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON ALL TABLES IN SCHEMA public FROM app;
   SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = 'app';
   ```
   Không dùng `default_transaction_read_only` ở mức cơ sở dữ liệu: nó chặn luôn tiến trình áp của subscription ở bước F, và phiên có thể `SET` lại. (Ghi lại các `GRANT` hiện có trước khi thu: `pg_dump --schema-only` hoặc `\dp`.)
5. `stashadow drain -source ... -timeout 2m`: đọc `pg_current_wal_lsn()` rồi chờ tới khi slot đã xác nhận (công cụ chỉ xác nhận sau khi STASK commit) tới vị trí đó. In `drained`.
6. Dừng `stashadow run` (SIGTERM). `stashadow verify` (không cần `-retries`: nguồn đang đứng) **phải khớp tuyệt đối**; lệch thì dừng, không chuyển, mở lại quyền ghi (bước G).
7. `stashadow sync-sequences -from <PG> -to <STASK>`: sao vị trí mọi sequence (nhân bản logic không mang sequence). Không làm bước này thì STASK cấp lại `id` đã dùng.
8. `stashadow drop` (xoá slot/publication cũ trên PostgreSQL, tránh giữ WAL).

**C. Bật nhân bản ngược trước khi mở ghi vào STASK** (để quay lại được)
9. Trên STASK: `CREATE PUBLICATION failback FOR TABLE ...`. Trên PostgreSQL: `CREATE SUBSCRIPTION failback CONNECTION 'host=stask port=5432 user=app dbname=stask' PUBLICATION failback WITH (copy_data = false)`. `copy_data=false` vì hai bên đang giống hệt. Đây là nhân bản logic gốc của PostgreSQL đọc từ walsender của STASK (kiểm bằng subscriber PG thật, cũng ở `accept/pgsub.sh`).

**D. Chuyển**
10. Trỏ ứng dụng sang STASK (DSN nhiều host của §3.3 nếu dùng cụm). Từ đây STASK là nơi ghi; PostgreSQL theo sau qua subscription.
11. `stashadow verify -a <STASK> -b <PG> -retries 30` định kỳ: khớp khi hết trễ.

**E. Quay lại (failback) nếu cần**
12. Đóng băng STASK: `REVOKE INSERT, UPDATE, DELETE ON ... FROM app` (STASK) và ngắt phiên của role đó. (Bài diễn tập kiểm: ghi bị từ chối sau khi thu quyền.)
13. Chờ PostgreSQL bắt kịp: `stashadow verify -a <STASK> -b <PG> -retries 60` khớp.
14. `DROP SUBSCRIPTION failback` trên PostgreSQL (xoá luôn slot trên STASK).
15. `stashadow sync-sequences -from <STASK> -to <PG>` (id mà STASK đã cấp).
16. Trả quyền ghi cho `app` trên PostgreSQL (`GRANT`), trỏ ứng dụng về PostgreSQL. Bài diễn tập kiểm: PostgreSQL cấp `id` lớn hơn id cuối của STASK.
17. Muốn tiếp tục bóng: `stashadow run -reset ...` (chụp lại).

**F. Tránh vòng lặp**: không bao giờ chạy đồng thời `stashadow run` (PG → STASK) và subscription (STASK → PG) trên cùng bảng: bước 6 và 8 phải xong trước bước 9.

### 2.6 Giới hạn
- Chưa thử trên dữ liệu thật của người dùng (kho 30 đến 50 triệu SIM): bài diễn tập dùng vài nghìn dòng. Tốc độ áp và thời gian ảnh chụp ban đầu trên cỡ thật thuộc danh sách đo cuối (D32).
- `TRUNCATE` được mang qua; giao dịch chuẩn bị hai pha, large object, kiểu mảng/enum/composite không hỗ trợ.
- `stashadow verify` tin vào biểu diễn văn bản của cột giống nhau trên hai engine (đã đúng trên CRM, `parity.py`); cột `float` hay `numeric` chênh cách in sẽ báo lệch giả: dùng `stashadow compare` (chuẩn hoá theo kiểu) để kiểm lại bảng đó.
- Đóng băng STASK ở bước 12 dựa trên quyền của role; một phiên superuser/`-pg-password` chung không bị chặn. Dùng role riêng cho ứng dụng.

---

## 3. Cụm Raft 3 nút trong production

### 3.1 Khởi động

Mỗi nút một thư mục dữ liệu **riêng** và cùng `-segment-mb`:

```sh
# nút 1 (nút 2, 3: đổi -raft-id, -dir, -raft-listen, -pg-addr)
staskd -dir /data/stask -addr 10.0.0.1:8080 -pg-addr 10.0.0.1:5432 \
       -pg-tls-cert n1.crt -pg-tls-key n1.key -pg-auth scram -pg-password-file /etc/stask/pg.pw \
       -raft-id 1 -raft-listen 10.0.0.1:7000 \
       -raft-peers 1=10.0.0.1:7000@10.0.0.1:8080,2=10.0.0.2:7000@10.0.0.2:8080,3=10.0.0.3:7000@10.0.0.3:8080 \
       -repl-tls-cert n1.crt -repl-tls-key n1.key -repl-tls-ca ca.crt -repl-tls-client-auth \
       -repl-secret-file /etc/stask/repl.secret -repl-require-secure
```

Cổng Raft dùng chung cơ chế bảo mật của cổng nhân bản (docs/06): mTLS + bí mật chung; cổng Raft không phải loopback mà không xác thực thì `staskd` từ chối chạy. `-gc` bị tắt trong cụm; `-archive-dir` dùng được (mỗi node một thư mục, docs/06 §5.2b). Đổi thành viên cụm: `staskctl cluster add|remove` (docs/06 §5.2b).

### 3.2 Nút báo đúng trạng thái đọc/ghi

Driver tìm leader qua trạng thái của nút (`TestHARoleReporting`, `TestHALibpqDocker`):

| Nút | `pg_is_in_recovery()` | `SHOW transaction_read_only` | ParameterStatus lúc kết nối | Ghi |
|---|---|---|---|---|
| leader | `false` | `off` | `in_hot_standby=off`, `default_transaction_read_only=off` | chạy |
| follower, hoặc leader chưa sẵn sàng | `true` | `on` | `in_hot_standby=on`, `default_transaction_read_only=on` | SQLSTATE **25006** (`cannot execute INSERT in a read-only transaction`), chưa chạy gì |

Replica bất đồng bộ (`-replica-of`) báo như follower. `GET /v1/ready?write=1` trả 200 chỉ ở leader (dùng cho bộ cân bằng tải gửi ghi); `GET /v1/ready` trả 200 ở mọi nút dùng được (đọc).

### 3.3 DSN phía client

| Driver | Chuỗi nối |
|---|---|
| libpq, psycopg2/3 | `host=n1,n2,n3 port=5432,5432,5432 dbname=stask user=app target_session_attrs=read-write` (đọc từ follower: `target_session_attrs=standby` hoặc `prefer-standby`) |
| pgjdbc | `jdbc:postgresql://n1:5432,n2:5432,n3:5432/stask?targetServerType=primary&loadBalanceHosts=false` (đọc: `secondary`, `preferSecondary`) |
| pgx (Go) | `postgres://app@n1:5432,n2:5432,n3:5432/stask?target_session_attrs=read-write` |

Đã kiểm: **libpq thật** (psql của image `postgres:16`; psycopg2 bọc cùng thư viện) tìm leader, tìm follower, và theo leader mới sau `kill -9` (`TestHALibpqDocker`, đặt `STASK_LIBPQ_DOCKER=1`); **pgx** trong `TestHARoleReporting`/`TestHAFailover`/`TestHASplitBrain`. **pgjdbc chưa kiểm** (máy không có Java): cơ chế của nó là `SHOW transaction_read_only` và tham số `in_hot_standby`, hai thứ nút đã báo, nhưng chưa chạy driver thật [chưa kiểm].

Ứng dụng phải chịu được lỗi khi leader đổi: kết nối đang mở bị đứt, giao dịch đang bay mất (kết quả `COMMIT` đang chờ có thể là không biết trước, giống mất mạng với PostgreSQL), nên thử lại toàn giao dịch với khoá idempotent. Connection pool nên kiểm `pg_is_in_recovery()` khi lấy kết nối cũ (`SELECT NOT pg_is_in_recovery()` làm validation query) hoặc đặt `maxLifetime` ngắn.

### 3.4 Mất leader, split-brain, khởi động lại cuốn chiếu

| Test | Kết quả |
|---|---|
| `TestHAFailover` (client chỉ dùng DSN nhiều host, `kill -9` leader hai lần) | client tiếp tục ghi sau ~1,4 đến 2,8 s mà không đổi gì; mọi ghi đã xác nhận có trên cả 3 nút |
| `TestProcChaos` (sẵn có, 3 tiến trình, kill/chia mạng một chiều và hai chiều) | không ghi xác nhận nào mất (số lần chạy mới: Phụ lục OPS) |
| `TestHASplitBrain` (cô lập leader khỏi hai follower, hai lần, client ghi vào **cả ba nút** trực tiếp; giám sát mỗi 15 ms) | không bao giờ có hai leader ghi được cùng một term; leader bị cô lập không xác nhận ghi mà đa số không có; sau khi lành mạng, mọi ghi xác nhận có trên cả 3 nút |
| `TestHARollingRestart` (SIGTERM từng nút: follower trước, leader cuối, client dùng DSN) | client có tiến triển sau mỗi bước; không mất ghi |

Cách an toàn của split-brain: ghi chỉ được xác nhận sau khi đa số fsync (leader cô lập không có đa số nên request chờ rồi `ErrUnknownOutcome`); leader mất đa số tự xuống sau check-quorum và khởi động lại tiến trình (cắt phần log chưa commit) trước khi trả lời bất kỳ request nào đang chờ. **Giới hạn**: đọc từ một nút không tuyến tính hoá (leader cũ vừa bị cô lập có thể trả dữ liệu cũ vài trăm ms đến hết check-quorum); đọc-ghi-của-mình dùng LSN (docs/06 §2a).

**Quy trình khởi động lại cuốn chiếu / nâng cấp binary** (mỗi lượt một nút, không bao giờ hai nút cùng lúc):
1. `staskctl status -url http://n1:8080,http://n2:8080,http://n3:8080`: cả ba `ready=true`, biết ai là leader.
2. Với từng **follower**: `SIGTERM`, đợi tiến trình thoát, thay binary, khởi động lại **cùng cờ và cùng thư mục**, đợi `staskctl status` cho thấy nút `ready=true` và `applied` gần `commit` rồi mới sang nút kế.
3. Leader sau cùng: `SIGTERM` (một lần bầu lại ~1,5 đến 2 s, client DSN tự sang leader mới), thay binary, khởi động lại.
4. Không xoá thư mục dữ liệu của một nút đang là thành viên (§9).
Định dạng đĩa và entry Raft phải tương thích giữa hai phiên bản trong lúc trộn: kiểm ghi chú phát hành; đổi định dạng thì quy trình này không dùng được (AGENT_PROTOCOL: đổi định dạng phải dừng và báo cáo). Test `TestHARollingRestart` dùng cùng một binary: nó kiểm quy trình, không kiểm khả năng tương thích giữa hai phiên bản.

### 3.5 Sao lưu và khôi phục cụm

Sao lưu: `staskctl backup -cluster -from n1:7000,n2:7000,n3:7000 -out /backup/stask` (tự tìm leader, lấy ảnh chụp nhất quán: checkpoint + segment + chỉ số Raft). Khôi phục (mất toàn cụm hoặc nhân bản sang cụm mới): trên **mỗi** nút mới, thư mục trống: `staskctl restore -backup /backup/stask/<time> -dir /data/stask-nodeN`, rồi khởi động ba nút với cùng cờ `-raft-*`; mỗi nút nhận ảnh chụp ở lần khởi động đầu. Đã kiểm (`TestHAClusterBackup`): sao lưu giữa lúc client đang ghi, khôi phục sang ba thư mục trống, cụm mới có đủ mọi ghi đã xác nhận trước lúc bắt đầu sao lưu, **không** có ghi sau đó, nhận ghi mới, các nút hội tụ; và `TestHAClusterBackupCLI` (qua `staskctl`, kể cả kiểm khôi phục tự động). `RestoreNode` từ chối thư mục đang dùng.

---

## 4. Sao lưu, kiểm khôi phục tự động, khôi phục: `staskctl`

```sh
staskctl backup      -from LEADER_REPL_ADDR -out DIR            # nút đơn: base backup (cần -repl-addr trên staskd)
staskctl backup      -cluster -from n1:7000,n2:7000,n3:7000 -out DIR
staskctl verify      -backup DIR/20261031T020000Z               # kiểm khôi phục
staskctl restore     -backup DIR/20261031T020000Z -dir NEWDATADIR
staskctl backup-loop -from ... -out DIR -interval 6h -keep 7 -metrics-file /var/lib/node_exporter/textfile/stask_backup.prom
staskctl status      -url http://n1:8080,http://n2:8080
```

(`-repl-secret-file`, `-repl-tls-ca`, `-repl-tls-cert/-key`, `-repl-require-secure` như `staskd` cho kết nối sao lưu.)

- Mỗi bản sao lưu là một thư mục có `manifest.json` (sha256 của từng file, loại, LSN, chỉ số Raft, kích thước).
- **`verify` là bài khôi phục thật** (giống chạy `pg_restore` vào DB tạm): kiểm manifest, chép sang thư mục tạm, **khởi động `staskd` trên bản chép**, đợi `/v1/ready`, chạy quét CRC toàn bộ (`/v1/admin/scrub`), đọc số dòng từng bảng, báo JSON (`-json`), thoát khác 0 nếu hỏng. Bản hỏng bị đổi tên `.FAILED` và không tính vào `-keep`. Đã kiểm (`TestBackupVerifyRestore`): file đổi sau khi sao lưu bị manifest bắt; checkpoint hoặc log hỏng sẵn lúc sao lưu (manifest được làm khớp) vẫn bị bắt (tiến trình khôi phục thoát lúc khởi động, hoặc quét CRC thất bại).
- **Lịch**: `backup-loop` chạy sao lưu, kiểm khôi phục, tỉa theo `-keep`, rồi ghi file Prometheus (textfile collector): `stask_backup_last_run_ok`, `stask_backup_last_success_timestamp_seconds`, `stask_backup_consecutive_failures`, `stask_backup_restore_test_ok`, `stask_backup_restore_start_seconds`, `stask_backup_bytes`, `stask_backup_lsn`, `stask_backup_duration_seconds` (`TestBackupLoopMetrics`). Chạy bằng systemd service hoặc cron với `-once` (thoát 1 khi hỏng).
- **PITR nút đơn**: base backup + `-archive-dir` của staskd (lưu liên tục các segment) + `-restore-archive`, `-recovery-target-time|-lsn` (docs/05 §14, `accept/pitr.sh`). Cụm Raft: `-archive-dir` ở mỗi node + `staskctl backup -cluster` + `staskctl restore -standalone` + `-restore-archive` (docs/06 §5.2b, `TestHAClusterPITR`); khôi phục ra một nút đơn, chưa tự dựng lại cụm từ đó [giới hạn §9].
- Khôi phục nút đơn: `staskctl restore -backup ... -dir NEW`, `staskd -dir NEW -segment-mb N` (N in sẵn).
- Kiểm tra dung lượng: sao lưu đầy đủ mỗi lần; `-keep` × kích thước DB phải vừa đĩa đích.

---

## 5. Giám sát

### 5.1 Endpoint
| Endpoint | Ý nghĩa | Xác thực |
|---|---|---|
| `GET /v1/health` | sống (engine không bị lỗi ghi) | công khai |
| `GET /v1/ready` | sẵn sàng: engine tốt, các view dẫn xuất đã bắt kịp, trong cụm đã biết leader; `?write=1` thêm: nút nhận ghi | công khai |
| `GET /metrics` | Prometheus | token (chỉ đọc đủ) |
| `GET /v1/cluster` | vai trò, term, leader, commit, applied | token |
| `GET /v1/status` | JSON chi tiết (bảng, view, nhân bản) | token |

### 5.2 Số đo mới trong `/metrics` (ngoài các số sẵn có: `stask_commits_total`, `stask_fsync_*`, `stask_wal_*`, `stask_sql_statement_duration_seconds`, `stask_pg_*`, `stask_scrub_*`...)
| Nhóm | Metric |
|---|---|
| độ trễ commit | `stask_commit_latency_seconds` (histogram; từ lúc gửi tới lúc được xác nhận, gồm chờ đa số trong cụm) |
| hàng đợi | `stask_commit_queue_depth` (sức chứa 4096) |
| nhân bản | `stask_read_only`, `stask_is_replica`, `stask_replica_lag_seconds` (replica), `stask_replication_lag_bytes{replica}`, `stask_replication_connected{replica}` (leader), `stask_cdc_slot_lag_bytes{slot}` |
| cụm Raft | `stask_raft_is_leader`, `stask_raft_writable`, `stask_raft_leader_known`, `stask_raft_term`, `stask_raft_lease`, `stask_raft_commit_index`, `stask_raft_applied_index`, `stask_raft_apply_lag_entries`, `stask_raft_snapshots_installed_total` |
| view dẫn xuất | `stask_view_lag_seconds{view}`, `stask_view_ready{view}` |
| bộ nhớ theo cấu trúc | `stask_structure_bytes{table,structure="primary_index"|"columnar"}`, cộng `stask_go_heap_alloc_bytes` |
| lịch sử | `stask_history_store_bytes{kind="disk"|"memory"}` (khi có `-hist-store`) |
| làn (admission) | `stask_lane_slots`, `stask_lane_heavy_slots`, `stask_lane_in_use`, `stask_lane_heavy_in_use`, `stask_lane_waiting`, `stask_lane_wait_seconds_total{class}`, `stask_lane_queued_total{class}` |
| sao lưu | file `staskctl backup-loop` (§4) |
| bóng | `/metrics` của `stashadow run -metrics-addr` (§2.2) |

Thiếu so với yêu cầu: kích cỡ index phụ và search/trigram theo bảng (chưa có accessor rẻ), `stask_structure_bytes` chỉ có chỉ mục chính và cột. Độ trễ commit là phía engine; độ trễ truy vấn SQL đã có từ trước (`stask_sql_statement_duration_seconds`).

### 5.3 Ngưỡng cảnh báo đề xuất

Các ngưỡng là điểm xuất phát chưa hiệu chỉnh trên tải thật; chỉnh sau khi đo (D32).

| Cảnh báo | Điều kiện | Mức | Làm gì |
|---|---|---|---|
| Nút ngừng ghi | `stask_up == 0` 1 phút | critical | §8.1 |
| Không có leader | `sum(stask_raft_is_leader) == 0` hơn 15 s (hoặc `stask_raft_leader_known == 0` 15 s) | critical | §8.2 |
| Hai leader | `sum(stask_raft_writable) > 1` hơn 5 s | critical | giữ nguyên, báo; không xảy ra trong test |
| Nút tụt sau | `stask_raft_apply_lag_entries > 1000` 5 phút | warning | đĩa/mạng nút đó |
| Commit chậm | p99 từ `stask_commit_latency_seconds` > 100 ms 10 phút (cụm: > 250 ms) | warning | đĩa chậm (`stask_fsync_slow_total` tăng), quá tải |
| Hàng đợi commit | `stask_commit_queue_depth > 2048` 1 phút | warning | quá tải ghi |
| fsync chậm | `increase(stask_fsync_slow_total[5m]) > 10` | warning | đĩa |
| Trễ replica | `stask_replica_lag_seconds > 30` 5 phút, hoặc `stask_replication_connected == 0` 2 phút | warning | §8.4 |
| View dẫn xuất tụt | `stask_view_lag_seconds > 5` 5 phút, hoặc `stask_view_ready == 0` sau khởi động 10 phút | warning | planner sẽ đọc từ core, chậm hơn |
| Đĩa log | `stask_wal_disk_bytes` > 70% đĩa | warning | slot nhân bản/CDC giữ log (`stask_cdc_slot_lag_bytes`), archive |
| Quét CRC | `increase(stask_scrub_errors_total[1d]) > 0` | critical | khôi phục từ bản sao lưu/replica |
| Sao lưu | `time() - stask_backup_last_success_timestamp_seconds > 2 * interval`, hoặc `stask_backup_last_run_ok == 0`, hoặc `stask_backup_restore_test_ok == 0` | critical | §8.6 |
| Đăng nhập thất bại | `rate(stask_pg_auth_failures_total[5m]) > 1` | warning | tấn công dò mật khẩu hoặc cấu hình sai |
| Bóng (chuyển đổi) | `stashadow_lag_seconds > 60` 5 phút; `stashadow_streaming == 0` 2 phút; `increase(stashadow_anomalies_total[5m]) > 0` | warning | §8.5; anomalies > 0 nghĩa là hai bên đã lệch |
| Slot trên PostgreSQL nguồn | `pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn) > 10 GB` | critical | công cụ bóng chết lâu; PostgreSQL sắp đầy đĩa |

---

## 6. Nhật ký

`staskd -log-level debug|info|warn|error` (mặc định `info`), `-log-format text|json` (mặc định `text`). JSON: một đối tượng mỗi dòng `{"time","level","msg"}`. Mã nguồn dùng gói `log` chuẩn nên cấp độ của một dòng được suy ra: tiền tố `debug:`, `warn:`, `error:` nếu có; không có thì dòng chứa "failed", "error", "refused", "corrupt", "panic", "fatal" là `warn`, còn lại `info` (một số dòng `log.Fatal` vì thế hiện `info`/`warn` chứ chưa phải `error`: giới hạn). Cờ đặt được trong file `-config`. Câu lệnh chậm: `-slow-query` (mật khẩu của `CREATE/ALTER ROLE` được che, `TestRolePasswordNotLogged`).

---

## 7. Bảo mật: rà soát (D)

| Hạng mục | Tình trạng khi rà | Xử lý |
|---|---|---|
| TLS trên cổng PG | Có (`-pg-tls-cert/-key`, TLS >= 1.2, `-pg-tls-require`, direct SSL, chống buffer-stuffing); test `TestTLSRequire`, `TestTLSOptional`, `TestDirectSSL` | không đổi |
| SCRAM | Có SCRAM-SHA-256 và -PLUS (`-pg-auth scram`), role có verifier luôn chứng minh mật khẩu | không đổi |
| Mật khẩu role trong nhật ký / `pg_stat_activity` | `CREATE/ALTER ROLE` được che ở slow log và `pg_stat_activity`; `pg_authid.rolpassword` hiện `********` | **thêm test** `TestRolePasswordNotLogged`; không có gì rò |
| Mật khẩu chung nằm trên dòng lệnh | `-pg-password` hiện trong `ps` và `/debug/pprof/cmdline` | **thêm** `-pg-password-file` (và nhắc dùng `-config`) |
| Mật khẩu cleartext qua mạng | `-pg-auth password` (mặc định khi có `-pg-password`) gửi cleartext; trên TLS thì ổn | **thêm cảnh báo lúc khởi động** khi cổng không loopback, mật khẩu cleartext, không TLS |
| `-pg-insecure` | `-pg-addr` không loopback mà không có mật khẩu thì staskd từ chối chạy (`CheckBind`); `-pg-insecure` ép chạy | **thêm cảnh báo** `warn:` khi dùng nó trên địa chỉ ngoài loopback; chạy `-pg-insecure` nghĩa là ai chạm được cổng đều là superuser |
| Danh sách cho phép kiểu pg_hba | Không có (chỉ `-pg-tls-require`) | **thêm** `-pg-hba FILE` (`internal/pgwire/hba.go`): `host\|hostssl\|hostnossl DATABASE USER ADDRESS METHOD`, khớp đầu tiên thắng, không khớp = từ chối `28000` trước xác thực; METHOD `reject\|trust\|password\|scram-sha-256` đồng thời là yêu cầu tối thiểu với `-pg-auth` (không đủ thì staskd không khởi động, file không bao giờ lặng lẽ làm yếu xác thực). Chưa hỗ trợ: tên máy, nhóm `+role`, file danh sách. `TestHBAMatch`, `TestHBAEnforced` |
| Dò mật khẩu | Không giới hạn | **thêm** làm chậm theo địa chỉ IP: sau hai lần sai, lần thử kế chờ 50 ms nhân đôi tới 2 s, thành công thì xoá; từ chối bởi `-pg-hba` cũng tính vào `stask_pg_auth_failures_total`. `TestAuthThrottle` |
| Cổng nhân bản / Raft | mTLS + bí mật chung (docs/06) | không đổi |
| HTTP API | token, TLS, `/v1/health` và `/v1/ready` công khai (chỉ nói được/không) | `/v1/ready` không lộ lý do chi tiết của lỗi engine |

**Còn hở** (không làm trong nhánh này): làm chậm theo IP không chặn kẻ dùng nhiều IP; nạp lại chứng chỉ bằng SIGHUP chưa có (đổi chứng chỉ phải khởi động lại, trong cụm thì làm cuốn chiếu §3.4); thu hồi chứng chỉ (CRL/OCSP) chưa có; một mật khẩu chung cho mọi nút/đối tác nhân bản; nhật ký kiểm toán đăng nhập (ai, khi nào) chưa có ngoài bộ đếm.

---

## 8. Quy trình xử lý sự cố

### 8.1 Nút báo `stask_up == 0` hoặc `/v1/health` 503
Engine gặp lỗi ghi (đĩa đầy, I/O) và dừng ghi. Xem nhật ký (dòng `failed`), `df -h`. Giải phóng đĩa rồi khởi động lại tiến trình (recovery cắt phần đuôi hỏng). Trong cụm: các nút khác vẫn phục vụ; nút này tự xuống và khi trở lại sẽ được nạp tiếp từ leader.

### 8.2 Cụm không có leader
`staskctl status`; nếu ít nhất 2 nút sống mà vẫn không có leader sau 10 s: kiểm mạng giữa các nút (cổng Raft `-raft-listen`), kiểm đồng hồ không nhảy, kiểm nhật ký `cluster:`. Một nút còn lại không thể bầu được (cần đa số). Hai nút chết: dựng lại từ bản sao lưu cụm (§3.5) hoặc khởi động lại nút cũ **với thư mục cũ**.

### 8.3 Một nút không theo kịp / bị loại
Thường tự phục hồi bằng snapshot từ leader (nhật ký `fetched a snapshot`). Nút hỏng đĩa: **chưa thay được** bằng nút mới (không có đổi thành viên); khôi phục cả cụm từ bản sao lưu (§3.5) khi nút không thể sửa.

### 8.4 Replica bất đồng bộ trễ
`stask_replication_lag_bytes`; replica đứt quá `KeepFor` (10 phút) nhận lỗi "cần base backup mới": dựng lại bằng `staskd -backup-from`.

### 8.5 Bóng (shadow) trễ hoặc dừng
`stashadow_streaming == 0`: xem nhật ký công cụ; lỗi "slot busy" là walsender cũ chưa thoát (tự thử lại). `anomalies > 0`: hai bên đã lệch; `stashadow verify` tìm bảng lệch, `compare` tìm dòng lệch, sửa bằng `-upsert` (chế độ sửa) hoặc `-reset` cho bảng đó. Công cụ dừng lâu: slot giữ WAL trên PostgreSQL; nếu không dùng nữa thì `stashadow drop`.

### 8.6 Sao lưu thất bại hoặc kiểm khôi phục thất bại
`staskctl backup-loop` giữ nguyên các bản tốt cũ. Đọc thông điệp lỗi trong nhật ký của nó; bản hỏng đổi tên `.FAILED`. Hỏng ở bước `manifest` = đĩa đích hỏng hoặc file bị sửa; hỏng ở "exited at start" hoặc "scrub" = dữ liệu nguồn có thể đã hỏng: chạy ngay `POST /v1/admin/scrub?wait=1` trên nguồn.

### 8.7 Đĩa của nút gần đầy
`stask_wal_disk_bytes`, `stask_cdc_slot_lag_bytes`: một slot CDC hay replica đã tắt giữ log. Xoá slot không dùng (`pg_drop_replication_slot`), đặt `-slot-max-lag-mb`.

---

## 9. Giới hạn và việc còn lại (trung thực)

| Mục | Ghi chú |
|---|---|
| Đổi thành viên cụm (thêm/bớt/thay nút) | Chưa làm. Tập bỏ phiếu cố định. Không xoá thư mục dữ liệu của nút đang là thành viên (nó quên phiếu bầu và có thể bỏ phiếu hai lần trong một term). Thiết kế định làm ở docs/06 §5.4. Hệ quả: nút hỏng đĩa không thay được tại chỗ; đường thoát là khôi phục cụm từ sao lưu |
| PITR cho cụm | Có (§5.2b docs/06); khôi phục ra nút đơn, chưa có lệnh dựng lại cụm 3 node từ bản khôi phục; chưa thử với kho hàng chục GB |
| Đọc tuyến tính hoá | Không có (đọc ở bất kỳ nút nào có thể cũ); đọc-ghi-của-mình dùng LSN |
| pgjdbc | Chưa chạy driver thật (không có Java trên VM) |
| Hiệu năng cụm | Độ trễ p50 3 đến 4 lần một nút (docs/06); không đo lại ở đây (D32: đo dồn về cuối, `perf-run.sh`) |
| Khởi động lại cuốn chiếu khác phiên bản | Chỉ kiểm cùng binary; tương thích giữa hai phiên bản chưa kiểm |
| Bóng trên dữ liệu thật | Chưa chạy trên kho/CRM thật cỡ đầy đủ |
| Sao lưu cụm lớn | Ảnh chụp đi qua một kết nối, giữ từng segment trong bộ nhớ (docs/06 §5.4 "Snapshot lớn") |
| Log cấp độ | Suy ra từ nội dung (§6) |
| Cụm: ngưỡng cảnh báo | Chưa hiệu chỉnh trên tải thật |
