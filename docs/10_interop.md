# 10 — Liên thông: CDC, LISTEN/NOTIFY (trigger hoãn lại)

Một engine hiệu năng cao vẫn phải cắm được vào hệ thống xung quanh: đồng bộ sang search/cache/kho phân tích, outbox cho microservice, và đánh thức worker khi có việc. Tài liệu này ghi lại những gì đã làm, cách đo, và lý do hoãn trigger cùng stored procedure.

## 1. Chọn giao diện: bắt chước PostgreSQL, không tự nghĩ ra

STASK đã nói PG wire protocol (docs/05), nên CDC và NOTIFY cũng dùng đúng tên hàm, định dạng và ngữ nghĩa của PG. Nhờ vậy công cụ có sẵn dùng được ngay, không phải viết connector riêng.

- Logical decoding qua SQL: `pg_create_logical_replication_slot`, `pg_logical_slot_get_changes` / `_peek_changes`, `pg_replication_slot_advance`, `pg_drop_replication_slot`, view `pg_replication_slots` ([PG docs: logical decoding](https://www.postgresql.org/docs/current/logicaldecoding-example.html), [hàm quản trị replication](https://www.postgresql.org/docs/current/functions-admin.html#FUNCTIONS-REPLICATION)).
- Hai output plugin dạng văn bản:
  - `test_decoding`: định dạng của [contrib/test_decoding](https://www.postgresql.org/docs/current/test-decoding.html);
  - `wal2json` format-version 1 và 2: định dạng của [wal2json](https://github.com/eulerto/wal2json), plugin phổ biến nhất cho consumer viết bằng ngôn ngữ bất kỳ.
- Thêm `pgoutput` qua walsender protocol (`START_REPLICATION`), định dạng mà subscriber PG (`CREATE SUBSCRIPTION`) và Debezium mặc định dùng ([Debezium PG connector](https://debezium.io/documentation/reference/stable/connectors/postgresql.html)). Chi tiết ở mục 6.
- `LISTEN` / `UNLISTEN` / `NOTIFY` / `pg_notify()` theo [ngữ nghĩa PG](https://www.postgresql.org/docs/current/sql-notify.html), gửi qua thông điệp NotificationResponse `'A'` của wire protocol, nên `pgx WaitForNotification`, JDBC `getNotifications`, psycopg `conn.notifies` dùng được nguyên.
- Thêm HTTP NDJSON (`GET /v1/cdc/{slot}/stream`, `POST /v1/cdc/{slot}/ack`) cho consumer không có driver PG. Mô hình "luồng thay đổi + xác nhận" giống changefeed của CockroachDB ([changefeed messages](https://www.cockroachlabs.com/docs/stable/changefeed-messages)).

## 2. Thiết kế CDC

**Slot** (`internal/core/cdc.go`):
- Là một con trỏ bền vào log, lưu ở `<dir>/slots/<tên>.json` (ghi tmp, fsync, rename, fsync thư mục).
- Slot giữ hai thứ để không bị thu hồi:
  - các segment log từ vị trí slot trở đi (qua `Shipper`, cùng cơ chế giữ log cho replica);
  - một snapshot GC tại LSN của slot. Nhờ vậy phiên bản cũ của một dòng vẫn đọc được để phân biệt INSERT với UPDATE, và DELETE dòng có thật với DELETE dòng không tồn tại.
- Giới hạn tụt hậu `-slot-max-lag-mb` (mặc định 0 = không giới hạn), như [`max_slot_wal_keep_size`](https://www.postgresql.org/docs/current/runtime-config-replication.html#GUC-MAX-SLOT-WAL-KEEP-SIZE). Vượt ngưỡng thì slot bị đánh dấu `lost` và thả log ra; consumer phải tạo slot mới và đồng bộ lại từ đầu. Mặc định giống PG: slot bị bỏ quên sẽ giữ log mãi, nên cần theo dõi `lag_bytes`.

**Giải mã**: đọc log thô từ vị trí slot, ghép chuỗi `TxPart` thành giao dịch, và chỉ phát ra giao dịch đã commit, theo đúng thứ tự commit. Thao tác PUT có `prev` trỏ tới phiên bản còn sống thì là U, ngược lại là I. DEL dòng đang sống là D; DEL dòng không có thì bỏ qua. Để biết phiên bản `prev` còn sống hay không, chỉ cần đọc byte loại op của nó (`wal.OpKindAt`), không giải mã cả bản ghi.

**Ánh xạ sang khái niệm PG**:
- LSN của PG ↔ vị trí log `seg/off`, in dạng `%X/%X`. So sánh như số 64 bit, vẫn đúng thứ tự.
- `xid` ↔ commit LSN của STASK (tăng dần, duy nhất).
- `lsn` của dòng COMMIT là **cuối** bản ghi commit, nên `pg_replication_slot_advance(slot, lsn_của_commit)` tiêu thụ đúng giao dịch đó.

**Giao nhận**: ít nhất một lần (at-least-once), giống PG. Stream không tự tiến slot. Consumer xác nhận LSN commit đã áp dụng xong; mở lại stream thì bắt đầu ngay sau giao dịch đã xác nhận cuối cùng. Mỗi slot chỉ có một consumer tại một thời điểm (consumer thứ hai nhận lỗi "active for another consumer", HTTP 409 khi chưa bắt đầu stream).

**Hook commit** `Tx.OnCommit(fn)`: chạy trên goroutine cài đặt duy nhất, theo thứ tự LSN, sau khi giao dịch hiển thị. NOTIFY dùng hook này. Hook có thể chạy ngay **sau** khi `Commit` trả về, vì commit được trả lời khi đã bền còn cài đặt xảy ra muộn hơn một chút.

## 3. Thiết kế LISTEN/NOTIFY (`internal/sql/notify.go`)

- Chỉ giao khi commit, theo thứ tự commit. Giao dịch rollback thì không giao gì.
- Các cặp kênh+payload trùng nhau trong cùng giao dịch chỉ giao một lần.
- `LISTEN`/`UNLISTEN` trong giao dịch có hiệu lực khi commit.
- Payload phải < 8000 byte. Không bền: listener mất kết nối thì mất thông báo, giống PG.
- **Khác PG một điểm**: PG có hàng đợi chung 8 GB, đầy thì `NOTIFY` báo lỗi. STASK có hàng đợi riêng cho mỗi listener, tối đa 100.000 thông báo; tràn thì bỏ thông báo của listener chậm đó và tăng `NotificationsDropped`. Lý do: một listener treo không được làm hỏng giao dịch của người khác.
- Gửi qua pgwire: khi phiên rảnh, ngay trước `ReadyForQuery`; hoặc bất đồng bộ bằng một goroutine notifier khi phiên đang chờ lệnh và không ở giữa một lô extended-query.

## 4. Kết quả đo

Cấu hình: 2 triệu dòng `orders` (bảng `columnar`), staskd 6 core (0–5), tải hỗn hợp đầy đủ của `staskmix` trên core 6–7 (20k read/s, 1k range, 8k update, 4k insert, 2k delete, 1k tx, 20 bulk, 0,5 agg), 30 s mỗi lượt. Các lượt có/không xen kẽ nhau. Script `scripts/cdcbench.sh`, kết quả `results/cdc/v4` (bản cuối) và `v1`–`v3` (trước khi tối ưu).

**CDC**: một consumer (`cmd/cdcbench`) đọc HTTP stream wal2json, xác nhận mỗi giây.

| | CPU server / 30 s | read p99 | update p99 | tx p99 | Consumer nhận | Trễ commit→nhận p50 / p99 / max |
|---|---|---|---|---|---|---|
| Không CDC (2 lượt) | 85,3–85,4 s | 33–54 ms | 18–34 ms | 35–43 ms | – | – |
| **Có CDC (2 lượt)** | **98,4–100,0 s** | 28–32 ms | 17 ms | 30–33 ms | **34,9k thay đổi/s** (495k tx), 0 sai thứ tự | **2,0 / 11 / 22–54 ms** |

- Chi phí là khoảng **+15% CPU server** (+13–15 s trên 85 s) để phát toàn bộ 35 nghìn thay đổi/s. Độ trễ của client không xấu đi: các khác biệt p99 nằm trong dao động giữa các lượt (lượt không-CDC thứ nhất có read p99 54 ms, thứ hai 33 ms).
- Consumer theo kịp: trễ p50 đứng yên ở 2 ms suốt lượt đo, và hai lượt nhận gần như cùng số thay đổi. Ở v1, trễ tăng dần tới 18 s.

**Quá trình tối ưu** (cùng tải, xem `results/cdc/v1`–`v3`):

| Bản | CPU thêm | Consumer nhận | Trễ p50 / p99 | Thay đổi |
|---|---|---|---|---|
| v1 | +25 s | 17k/s (tụt dần) | 8,9 s / 17,8 s | `json.Encoder` kiểm tra + nén lại từng thông điệp; flush mỗi commit; consumer giải mã mọi dòng |
| v2 | +22 s | 35k/s | 4,7 / 24–48 ms | ghi dòng bằng tay, gom flush (khi đuổi kịp log hoặc mỗi 10 ms), consumer chỉ giải mã dòng commit |
| v3/v4 | **+12–15 s** | 35k/s | **2,0 / 11 ms** | `prevLive` chỉ đọc byte loại op thay vì cả bản ghi; wal2json dựng bằng append, không qua `json.Marshal` |

Phần còn lại (profile `results/cdc/v3/cpu-on.pb.gz`): đọc log để phân loại I/U (`OpKindAt`, khoảng 5% CPU, phần lớn là syscall `pread` cho segment không còn được map) và dựng chuỗi JSON (khoảng 5%).

**NOTIFY**: 8 listener, 1 người gửi 1000 `pg_notify`/s, chạy cùng tải hỗn hợp (`cmd/notifybench`).

| | CPU server / 30 s | Giao | Mất / sai thứ tự | Trễ gửi→nhận p50 / p99 / p999 / max |
|---|---|---|---|---|
| 2 lượt | 89,2–89,8 s (+4 s) | 240.008 / 240.008 | 0 / 0 | **0,23 / 3,9 / 6,5–7,0 / 12–14 ms** |

Lượt NOTIFY thứ hai có read p99 197 ms, cao hơn hẳn các lượt khác. Lượt thứ nhất cùng cấu hình chỉ 37 ms, và CPU hai lượt như nhau, nên đây nhiều khả năng là dao động của máy hơn là do NOTIFY. Chưa kiểm chứng thêm.

## 5. Trigger và stored procedure: trigger thân SQL đã làm, còn lại hoãn

**Stored procedure / function do người dùng viết**: không làm.
- Logic nghiệp vụ trong DB khó test, khó version, khó quan sát, và trói ứng dụng vào một engine. Kinh nghiệm thực tế của người dùng (một lần trong 15 năm) khớp với xu hướng chung.
- Các hệ mới thường bắt đầu không có, rồi chỉ thêm dạng hạn chế khi bị ép bởi nhu cầu tương thích: CockroachDB mới có [PL/pgSQL từ v23.2](https://www.cockroachlabs.com/docs/stable/plpgsql); ClickHouse chỉ có [UDF dạng lambda](https://clickhouse.com/docs/sql-reference/statements/create/function).
- Chi phí lớn: một ngôn ngữ thủ tục đầy đủ, quyền hạn, và giới hạn tài nguyên khi chạy trong engine.

**Trigger**: dạng thân SQL **đã làm (2026-10-29, docs/05 §18)**, theo lý do dưới đây. Ngôn ngữ thủ tục vẫn không làm.
- Phần lớn nhu cầu "khi bảng X đổi thì làm Y" là đồng bộ ra ngoài hoặc đánh thức worker. CDC (bền, có thứ tự, at-least-once) cộng với NOTIFY đã làm được việc đó, theo mẫu [transactional outbox](https://microservices.io/patterns/data/transactional-outbox.html): ghi bảng outbox trong cùng giao dịch, rồi để CDC hoặc worker đọc ra.
- Trigger chạy **trong** đường ghi, nên mỗi trigger thêm độ trễ vào mọi giao dịch ghi bảng đó. Điều này đi ngược mục tiêu hiệu năng cân bằng của engine.
- Nếu sau này có ca di trú cụ thể cần trigger, chỉ làm dạng thân SQL như [SQLite](https://www.sqlite.org/lang_createtrigger.html) (BEFORE/AFTER, FOR EACH ROW, thân là các câu INSERT/UPDATE/DELETE/SELECT có `NEW`/`OLD`). Không làm ngôn ngữ thủ tục. CockroachDB cũng đi theo hướng thận trọng: [trigger mới ở dạng preview](https://www.cockroachlabs.com/docs/stable/triggers).

## 6. pgoutput, walsender, publication và REPLICA IDENTITY

**Walsender** (`internal/pgwire/walsender.go`): kết nối mở với `replication=database` nhận thêm các lệnh replication của PG ([streaming replication protocol](https://www.postgresql.org/docs/16/protocol-replication.html)), bên cạnh SQL thường:

| Lệnh | Hỗ trợ |
|---|---|
| `IDENTIFY_SYSTEM` | có (timeline 1, `xlogpos` = vị trí bền) |
| `CREATE_REPLICATION_SLOT name [TEMPORARY] LOGICAL plugin [...]` | có; cú pháp cũ và mới; không xuất snapshot (`snapshot_name` null); `USE_SNAPSHOT` và `TWO_PHASE` bị từ chối |
| `DROP_REPLICATION_SLOT name [WAIT]` | có |
| `START_REPLICATION SLOT name LOGICAL lsn (options)` | có: CopyBoth, XLogData `w`, keepalive `k` mỗi 10 s, standby status `r` (vị trí flush xác nhận slot), CopyDone |
| Replication vật lý, `BASE_BACKUP`, `TIMELINE_HISTORY`, `READ_REPLICATION_SLOT` | không (replica STASK dùng giao thức riêng, docs/06) |

**pgoutput** (`internal/sql/pgoutput.go`), theo [định dạng thông điệp](https://www.postgresql.org/docs/16/protocol-logicalrep-message-formats.html), proto_version 1–4:
- Gửi nguyên giao dịch, không stream giao dịch đang chạy dở. Tuỳ chọn `streaming` được chấp nhận nhưng không có tác dụng, điều mà giao thức cho phép.
- BEGIN chỉ được gửi khi giao dịch có ít nhất một thay đổi thuộc publication; giao dịch không liên quan không sinh thông điệp nào, giống PG ≥ 15. Keepalive báo vị trí đã giải mã xong, nên consumer vẫn xác nhận qua được các giao dịch đó.
- Relation được gửi trước thay đổi đầu tiên của mỗi bảng, và gửi lại khi replica identity của bảng đổi. Tuple dạng text, hoặc nhị phân khi `binary 'true'`.
- DELETE và UPDATE theo replica identity: `K` (khoá, các cột khác null) hoặc `O` (cả dòng cũ, khi FULL).

**TRUNCATE** (thêm 2026-10-29) được phát như PG: một giao dịch chứa một thay đổi. Dạng thay đổi theo plugin:
- test_decoding: `table public.t: TRUNCATE: (no-flags)`;
- wal2json format 2: `{"action":"T","schema":"public","table":"t"}`. Format 1 không phát, đúng như [wal2json](https://github.com/eulerto/wal2json): `actions` của format 1 không có truncate;
- pgoutput: thông điệp `T` (1 relation, không cờ), sau thông điệp Relation nếu chưa gửi.

Publication có `publish` gồm cả `truncate` theo mặc định, như PG; cột `pubtruncate` hiển thị giá trị này. DROP TABLE không thành sự kiện, giống PG.

**Publication**: `CREATE PUBLICATION p FOR TABLE a, b | FOR ALL TABLES [WITH (publish = 'insert, update, delete, truncate')]`, `DROP PUBLICATION [IF EXISTS]`, các view `pg_publication` và `pg_publication_tables`. Không phải giao dịch: có hiệu lực ngay, kể cả khi chạy trong BEGIN.

**REPLICA IDENTITY**: `ALTER TABLE t REPLICA IDENTITY {DEFAULT | FULL}`. FULL thì UPDATE/DELETE mang cả dòng cũ, ở cả bốn định dạng:
- test_decoding: `old-key: ... new-tuple: ...`;
- wal2json v2: `identity` chứa mọi cột;
- wal2json v1: `oldkeys` chứa mọi cột;
- pgoutput: `O`.

**Khác PG một điểm**: ở PG, replica identity quyết định WAL ghi gì vào lúc ghi. Ở STASK, slot đã giữ phiên bản cũ nên identity chỉ được đọc lúc giải mã: bảng không bật FULL thì đường ghi không tốn gì thêm. Hệ quả là identity đang có hiệu lực **lúc giải mã** quyết định nội dung. Các thay đổi đã ghi nhưng chưa được tiêu thụ trước một lệnh `ALTER ... FULL` cũng sẽ mang dòng cũ, tức là nhiều thông tin hơn chứ không thiếu.

Publication và replica identity lưu ở `<dir>/cdc.json`, chỉ trên leader, giống slot.

**Shim phiên bản**: kết nối replication báo `server_version` = 15.0, còn kết nối thường vẫn báo 16.0. Lý do: subscriber PG 16 nối vào publisher ≥ 16 sẽ đọc danh sách bảng bằng `pg_get_publication_tables(VARIADIC array_agg(...))` JOIN `pg_class`/`pg_namespace` ([subscriptioncmds.c](https://raw.githubusercontent.com/postgres/postgres/REL_16_STABLE/src/backend/commands/subscriptioncmds.c)), mà STASK không có. Với publisher bản 15, nó dùng `pg_publication_tables`, mà STASK đã có. Giao thức replication của hai bản giống nhau cho những gì STASK dùng.

**Sửa kèm**: số thực giờ in như `float8out` của PG (số mũ < -4 hoặc ≥ 15 mới dùng dạng mũ), thay vì `%g` của Go. Trước đây `1234567` bị in thành `1.234567e+06`. Ảnh hưởng tới mọi kết quả text qua pgwire và wal2json.

### Nghiệm thu

1. **Client độc lập** `accept/logrepl` (module riêng, dùng [`pglogrepl`](https://github.com/jackc/pglogrepl)) so từng thông điệp:
   - framing BEGIN/COMMIT, Relation, I/U/D với DEFAULT và FULL;
   - lọc theo publication, tuple text và nhị phân;
   - xác nhận rồi nối lại;
   - slot TEMPORARY bị xoá khi đóng kết nối;
   - consumer thứ hai bị từ chối (55006);
   - lỗi trước khi stream để lại kết nối dùng được.

   Kết quả: **PASS**.
2. **Subscriber PostgreSQL 16 thật** (`accept/pgsub.sh`: container `postgres:16`, `CREATE SUBSCRIPTION ... WITH (copy_data = false)` trỏ vào STASK), `results/accept/pgsub.txt`:
   - Từng câu lệnh (NULL, chuỗi có `'`, `\` và Unicode, giao dịch nhiều câu, rollback, bảng FULL, bảng không publish): hai bên giống nhau.
   - Tải hỗn hợp `staskmix` trên 200k dòng `orders`, 2 × 15 s, **khởi động lại STASK giữa chừng**. Subscriber tự nối lại, tiếp tục từ replication origin của nó, không lặp và không mất giao dịch: **520.007 dòng giống từng dòng** (so md5 toàn bảng). PG đuổi kịp 9,4 s sau khi tải dừng. Nguyên nhân của 9,4 s này chưa đo; giả thuyết là apply worker một luồng của PG.
   - `DROP SUBSCRIPTION` xoá slot bên STASK.
   - 16 giá trị float8 in giống hệt PG.

## 7. Giới hạn và hướng xử lý

| Giới hạn | Hướng xử lý |
|---|---|
| Debezium **chưa được thử thật**. Ngoài walsender, Debezium còn truy vấn nhiều catalog qua kết nối thường (`pg_type`, `pg_attribute`, `pg_index`, `information_schema`…), có thể STASK chưa có đủ. | **Làm ở bản Go nếu Debezium là yêu cầu thật**: chạy Debezium Server, ghi lại các truy vấn catalog nó gửi, bổ sung tới khi chạy được. Chưa làm vì subscriber PG và `pglogrepl` đã phủ giao thức; phần còn lại là catalog. |
| Slot, publication và replica identity chỉ có trên leader; failover làm mất chúng. | **Hoãn tới giai đoạn B ở docs/06** (Raft failover), không liên quan tới Rust. Khi có failover, chuyển chúng vào log để replica có cùng trạng thái, giống [failover slots của PG 17](https://www.postgresql.org/docs/17/logicaldecoding-explanation.html#LOGICALDECODING-REPLICATION-SLOTS-SYNCHRONIZATION). |
| DDL không thành sự kiện. | **Không cần làm riêng**: PG cũng không phát DDL qua logical decoding. Hiện STASK chỉ có `CREATE TABLE`, và bảng mới được mô tả bằng thông điệp Relation trước dòng đầu tiên. Khi có `ALTER TABLE ADD COLUMN`, Relation sẽ được gửi lại như PG. |
| Không xuất snapshot khi tạo slot (`USE_SNAPSHOT`, `copy_data = true`). Subscriber phải dùng `copy_data = false` và tự chép dữ liệu cũ. | **Làm ở bản Go cùng đợt Debezium** (Debezium cần nó cho snapshot ban đầu). STASK có sẵn snapshot theo LSN; còn thiếu `SET TRANSACTION SNAPSHOT` và `COPY ... TO STDOUT` cho tablesync. |
| Không stream giao dịch dở dang (`streaming on/parallel`). | **Không làm**, trừ khi có giao dịch rất lớn gây vấn đề bộ nhớ ở consumer. Giao thức cho phép server gửi nguyên giao dịch. |
| Publication/replica identity không theo giao dịch. | **Không làm**: đã ghi rõ ở trên; chỉ khác khi chạy chúng trong BEGIN rồi ROLLBACK. |
| NOTIFY không bền. | **Không làm**: giống PG; cần bền thì dùng CDC. |

Test: `internal/core/cdc_test.go` (gồm `TestCDCCatalog`), `internal/sql/cdc_test.go` (gồm `TestSQLPublicationsAndIdentity`), `internal/sql/pgfloat_test.go`, `internal/pgwire/pgwire_test.go` (`TestListenNotify`), `internal/server/cdc_test.go`, `internal/wal/wal_test.go`, cùng `accept/logrepl` và `accept/pgsub.sh`. Tất cả qua, unit test qua cả với `-race`.
