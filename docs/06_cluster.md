# 06 — Cluster: cách ly và điều hướng thao tác

Một máy, dù lớn đến đâu, cũng có giới hạn. Trên một máy, ở tải quá mức, aggregate và đọc điểm phải chia nhau CPU: chỉnh trọng số cgroup hay giới hạn cứng chỉ chọn được bên nào chịu thiệt. Cách triệt để là **đặt từng loại việc lên máy riêng** và **điều hướng từng thao tác tới máy hợp với nó**.

Các hệ đang chạy production làm theo hướng này:
- TiDB đặt bản sao dạng cột (TiFlash) làm Raft learner trên node riêng, nên OLAP không chặn OLTP ([TiFlash](https://docs.pingcap.com/tidb/dev/tiflash-overview/), [VLDB 2020](https://www.cs.purdue.edu/homes/csjgwang/CloudNativeDB/TiDBVLDB20.pdf)).
- CockroachDB cho phép đọc từ follower dưới closed timestamp ([follower reads](https://www.cockroachlabs.com/docs/stable/follower-reads)).
- TiKV và YugabyteDB dùng lease để leader đọc mà không cần một vòng mạng ([lease read](https://tikv.org/blog/lease-read/), [leader leases](https://www.yugabyte.com/blog/low-latency-reads-in-geo-distributed-sql-with-raft-leader-leases/)).

## 1. Vai trò node

| Vai trò | Nhận | Ghi chú |
|---|---|---|
| Leader | Mọi ghi (U/I/D, giao dịch), đọc điểm | Điểm commit duy nhất: giao dịch nằm trên đường ghi |
| Follower (giai đoạn B) | Log, bỏ phiếu xác nhận ghi; đọc điểm | Thay leader khi leader chết |
| Learner / replica phân tích | Log, không bỏ phiếu; aggregate, quét lớn | Không làm chậm commit; về sau có thể lưu dạng cột |

Ghi không tăng theo số node: mọi bản sao phải áp toàn bộ ghi. Đọc tăng theo số bản sao. Tăng sức ghi cần phân mảnh (giai đoạn C).

## 2. Giai đoạn A (đã cài đặt): replica bất đồng bộ

Mã: `internal/core/replica.go` (lõi), `internal/repl` (giao thức TCP), cờ `staskd -repl-addr` và `-replica-of`. Bảo mật cổng nhân bản (TLS, secret, mTLS): xem cuối mục này.

**Nhân bản vật lý, giống từng byte.** Replica ghi từng byte của log leader vào **đúng segment và offset** như trên leader. Nhờ vậy:
- Vị trí phiên bản (`Loc`, `ValOff`, `Prev`) có cùng nghĩa trên hai bên.
- Replica dựng chỉ mục bằng đúng cách recovery phát lại log (cùng code `replay`), không cần định dạng thứ hai.
- Leader chỉ gửi byte đã `fdatasync`, nên log của replica luôn là **tiền tố** của log leader. Leader sập không làm replica có dữ liệu mà leader không có.

**Khởi tạo.** Replica rỗng nhận một base backup: mọi segment tới điểm bền vững, rồi một checkpoint mới chụp. Sau đó replica recovery như một engine vừa khởi động lại. Thư mục chỉ thành replica khi `replica.json` được đổi tên vào chỗ (một thao tác rename).

**Áp log.** Mỗi khung dữ liệu được ghi vào log, `fdatasync`, giải mã, rồi cài vào **bản clone copy-on-write** của chỉ mục và công bố bằng một lần store nguyên tử. Người đọc trên replica không bao giờ khóa. Giao dịch lớn bị tách thành nhiều frame (`KindTxPart`, có thể vắt qua segment) chỉ được cài khi frame cuối tới.

**Thu hồi log.** GC chỉ chạy trên leader:
- Việc di chuyển phiên bản (`KindMove`) tới replica qua log.
- Riêng việc **xóa tombstone cũ khỏi chỉ mục không được ghi vào log**. Vì vậy, khi xóa một segment mà leader đã thu hồi, replica tự bỏ các tombstone còn trỏ vào đó.
- Replica chỉ xóa sau khi đã áp mọi thứ leader đã bền vững tại thời điểm báo, và sau khi ghi một checkpoint không còn tham chiếu segment đó.
- Nếu còn đầu phiên bản sống trỏ vào segment đó (không thể xảy ra nếu đúng thứ tự), replica từ chối xóa.

**Slot nhân bản**, như [replication slot của PostgreSQL](https://www.postgresql.org/docs/16/warm-standby.html#STREAMING-REPLICATION-SLOTS):
- Leader giữ log mà replica còn cần trong lúc replica mất kết nối, tối đa `KeepFor` (10 phút).
- Replica quay lại muộn hơn sẽ nhận lỗi rõ ràng ("cần base backup mới") thay vì dữ liệu sai.

**Đọc trên replica:**
- Ảnh chụp tại LSN replica đã áp: dữ liệu cũ hơn leader một khoảng bằng độ trễ nhân bản, `Engine.ReplicaLag`.
- Như sau khi leader khởi động lại, đọc as-of cần một phiên bản không phải đầu đã bị di chuyển sẽ trả `ErrSnapshotTooOld`.
- Ghi trên replica trả lỗi `read-only replica: writes go to the leader`.

**Đã kiểm chứng** (`internal/repl/repl_test.go`, có race detector):
- Base backup trong lúc leader đang ghi và chạy GC.
- Giao dịch 70.000 thao tác vắt qua nhiều segment.
- Khởi động lại replica giữa chừng.
- Leader thu hồi 37 segment, replica xóa theo 34.
- Ở mỗi điểm kiểm tra, toàn bộ bảng trên replica **trùng từng dòng** với leader tại cùng LSN (72.929 và 72.925 dòng), số dòng sống cũng trùng.
- Kiểm tra đầu-cuối qua `psql` trên hai tiến trình `staskd`: UPDATE, INSERT, DELETE trên leader hiện ngay trên replica; GROUP BY trên replica cho kết quả đúng; ghi vào replica bị từ chối.

### Bảo mật cổng nhân bản (P9 S1, đã cài đặt)

Mã: `internal/repl/security.go`, test `internal/repl/security_test.go`.

**Mối đe doạ.** Trước thay đổi này, ai kết nối được tới `-repl-addr` đều nhận được base backup, tức toàn bộ cơ sở dữ liệu, rồi stream mọi ghi mới. Ngược lại, replica áp bất cứ log nào do bên kia `-replica-of` gửi tới. Cần chống ba việc:
1. Kẻ lạ kéo dữ liệu từ leader (thiếu xác thực replica).
2. Nghe lén hoặc sửa log trên đường truyền (thiếu mã hoá và toàn vẹn).
3. Leader giả: kẻ chen giữa hoặc chiếm địa chỉ leader đẩy dữ liệu sai cho replica (thiếu xác thực leader).

**Các hệ thật làm thế nào:**
- PostgreSQL: dòng `hostssl ... replication ... scram-sha-256` trong [`pg_hba.conf`](https://www.postgresql.org/docs/current/auth-pg-hba-conf.html) bắt replica phải dùng TLS và chứng minh mật khẩu bằng SCRAM (mật khẩu không đi trên dây); phía standby đặt `sslmode=verify-full` trong `primary_conninfo` để kiểm cả CA lẫn tên máy của primary ([libpq SSL](https://www.postgresql.org/docs/current/libpq-ssl.html), [Hagander, replication security](https://www.hagander.net/talks/replication_security.pdf)). `verify-ca` không đủ vì chấp nhận mọi chứng chỉ do CA đó ký ([thảo luận trên pgsql-general](https://www.postgresql.org/message-id/1030654552.2676681.1599138579381%40mail.yahoo.com)).
- etcd: kết nối giữa các member dùng mTLS: `--peer-cert-file`, `--peer-key-file`, `--peer-trusted-ca-file`, `--peer-client-cert-auth`; một chứng chỉ peer dùng cho cả hai chiều (serverAuth + clientAuth) ([etcd transport security](https://etcd.io/docs/v3.6/op-guide/security/)).
- CockroachDB: mỗi node có node certificate do CA của cluster ký, dùng cho cả kết nối vào lẫn ra giữa các node; SAN phải chứa địa chỉ của node ([cockroach cert](https://www.cockroachlabs.com/docs/stable/cockroach-cert), [authentication](https://www.cockroachlabs.com/docs/stable/authentication)).
- Redis: replica đặt `tls-replication yes` và `masterauth`; master áp `tls-auth-clients` cho replica như cho client thường ([Redis TLS](https://redis.io/docs/latest/operate/oss_and_stack/management/security/encryption/)).
- MongoDB: xác thực nội bộ giữa các member bằng keyfile (secret dùng chung, chứng minh bằng SCRAM) hoặc x.509 ([internal authentication](https://www.mongodb.com/docs/manual/core/security-internal-authentication/)).

**Thiết kế chọn: hai lớp độc lập, như PostgreSQL (TLS + SCRAM) và Redis (TLS + masterauth), cộng tuỳ chọn mTLS như etcd.**

1. **TLS.** Leader đọc byte đầu tiên của kết nối: `0x16` là bản ghi bắt tay TLS, còn lại là giao thức thường (giống cách pgwire nhận direct SSL). Nhờ vậy một cổng phục vụ được cả hai, và có thể chuyển dần replica sang TLS. Replica có `-repl-tls-ca` thì luôn quay số TLS và kiểm chứng chỉ của leader theo kiểu `verify-full`: chữ ký CA **và** tên (mặc định là host trong `-replica-of`, IP thì so với IP SAN). Có `-repl-tls-client-auth` thì leader đòi chứng chỉ client do cùng CA ký (mTLS).
2. **Secret dùng chung, challenge-response hai chiều.** Secret không bao giờ đi trên dây, kể cả khi không có TLS:
   ```
   replica -> leader   magic "STASKREPL2"
   leader  -> replica  'N' u8 flags (1 = cần secret), nonce Ns (32 byte)
   replica -> leader   'U' nonce Nr (32 byte), HMAC-SHA256(secret, "stask-repl replica" Ns Nr B)
   leader  -> replica  'O' HMAC-SHA256(secret, "stask-repl leader" Ns Nr B)
   replica -> leader   'H' hello (như trước)
   ```
   - Leader so MAC bằng `hmac.Equal` (thời gian hằng), sai thì trả `authentication failed`.
   - Replica cũng kiểm MAC của leader. Nếu replica có secret mà leader báo không cần secret, replica **từ chối** (leader không chứng minh được mình, chống hạ cấp).
   - `B` là TLS exporter của kết nối (channel binding, [RFC 9266](https://www.rfc-editor.org/rfc/rfc9266)), rỗng khi không có TLS. Bằng chứng làm trên một kết nối TLS không dùng lại được trên kết nối khác, nên kẻ chen giữa không chuyển tiếp được.
   - Nonce mới mỗi lần, nên không phát lại được bằng chứng cũ.
3. **Chính sách của leader.** Một kết nối được coi là đã xác thực khi nó chứng minh được secret hoặc có chứng chỉ client đã kiểm. Leader có cấu hình xác thực thì từ chối mọi kết nối khác. Có mTLS mà không có secret thì kết nối không TLS bị từ chối (không có cách xác thực nó).
4. **Phòng thủ trước xác thực.** Bắt tay TLS, xác thực và hello phải xong trong 10 s. Khung đọc trước khi xác thực tối đa 4 KiB (trước đây 256 MiB), nên kẻ lạ không bắt leader cấp phát nhiều bộ nhớ được.
5. **Từ chối khởi động khi mở cổng mà không xác thực.** `-repl-addr` trên địa chỉ không phải loopback (kể cả `:port`, `0.0.0.0`) mà không có `-repl-secret`/`-repl-secret-file` hay `-repl-tls-client-auth` thì `staskd` dừng với lỗi rõ ràng, trừ khi có `-repl-insecure`. Loopback (`127.0.0.1`, `::1`, `localhost`) vẫn chạy như cũ, nên các script đo (`scripts/replcost.sh`, `scripts/mixcompare.sh`) không phải đổi.

Giao thức đổi magic từ `STASKREPL1` sang `STASKREPL2`: leader và replica phải cùng phiên bản (replica cũ nhận lỗi "not a replica", replica mới gặp leader cũ báo frame lạ).

**Cờ** (đặt được trong file `-config` với cùng tên, vd. `repl-secret-file = /etc/stask/repl.secret`):

| Cờ | Bên | Ý nghĩa |
|---|---|---|
| `-repl-secret` | cả hai | Secret dùng chung. Dòng lệnh thì người dùng khác thấy được qua `ps`, nên nên đặt trong file `-config` hoặc dùng `-repl-secret-file` |
| `-repl-secret-file` | cả hai | File chứa secret (bỏ khoảng trắng cuối). Không đặt cùng `-repl-secret` |
| `-repl-tls-cert`, `-repl-tls-key` | cả hai | Chứng chỉ PEM: của leader trên `-repl-addr`; chứng chỉ client của replica khi có `-replica-of`. Như etcd, một chứng chỉ (serverAuth + clientAuth) dùng được cho node vừa là replica vừa phục vụ replica khác |
| `-repl-tls-ca` | cả hai | CA PEM: replica dùng để kiểm leader (bật TLS phía replica); leader dùng để kiểm chứng chỉ client khi có `-repl-tls-client-auth` |
| `-repl-tls-server-name` | replica | Tên chứng chỉ leader phải mang (mặc định: host trong `-replica-of`) |
| `-repl-tls-client-auth` | leader | Đòi chứng chỉ client do `-repl-tls-ca` ký (mTLS) |
| `-repl-require-secure` | cả hai | Leader: từ chối replica không vừa TLS vừa đã xác thực (khởi động lỗi nếu thiếu chứng chỉ hoặc thiếu cách xác thực). Replica: bắt buộc có `-repl-tls-ca` |
| `-repl-insecure` | leader | Cho phép `-repl-addr` không phải loopback mà không xác thực replica |

Ví dụ cấu hình khuyến nghị (TLS verify-full + secret + mTLS):
```
# leader
staskd -repl-addr 10.0.0.1:17500 -repl-tls-cert leader.crt -repl-tls-key leader.key \
       -repl-tls-ca ca.crt -repl-tls-client-auth -repl-secret-file repl.secret -repl-require-secure
# replica
staskd -replica-of 10.0.0.1:17500 -repl-tls-ca ca.crt -repl-tls-cert replica.crt -repl-tls-key replica.key \
       -repl-secret-file repl.secret -repl-require-secure
```

**Đã kiểm chứng** (`internal/repl/security_test.go`, có race detector; CA và chứng chỉ sinh trong test):
- Secret đúng: base backup chạy; secret đọc từ file cũng chạy.
- Secret sai: `authentication failed`. Replica không có secret: `authentication required`. Replica có secret gặp leader không có: replica từ chối (`does not authenticate`).
- TLS kiểm theo IP SAN và theo tên chạy. CA khác: `certificate signed by unknown authority`. Đúng CA nhưng sai tên: `certificate is valid for ...` (verify-full).
- `-repl-require-secure`: replica không TLS bị từ chối (`requires TLS`); cấu hình thiếu TLS hoặc thiếu xác thực bị từ chối khi khởi tạo.
- mTLS: chứng chỉ client đúng CA chạy; không có chứng chỉ hoặc chứng chỉ do CA khác ký bị từ chối.
- Kiểm tra địa chỉ: loopback được; `:port`, `0.0.0.0`, IP mạng không có xác thực bị từ chối, trừ khi có `-repl-insecure` hoặc secret.
- Replica theo leader đầy đủ (base backup rồi stream) qua mTLS + secret + require: dữ liệu trùng từng dòng.
- `TestReplicaFollowsLeader` (không bảo mật, loopback) vẫn qua.

**Giới hạn và cách xử lý:**

| Giới hạn | Xử lý |
|---|---|
| Không có TLS thì secret chỉ xác thực lúc bắt đầu; dữ liệu sau đó đi dạng rõ và về lý thuyết có thể bị chèn gói TCP | Không làm: đây là thiết kế. Mạng không tin cậy thì bật TLS (hoặc `-repl-require-secure`) |
| Đổi chứng chỉ hay secret phải khởi động lại | Hoãn tới P5 (vận hành): nạp lại khi nhận SIGHUP |
| Chưa kiểm thu hồi chứng chỉ (CRL/OCSP) | Hoãn: dùng chứng chỉ ngắn hạn; xem lại cùng S4 |
| Không giới hạn số lần xác thực sai | Hoãn, làm cùng S5 (trễ tăng dần, khoá tạm theo IP) |
| Một secret cho mọi replica, không phân biệt từng replica | Hoãn: cần thì dùng mTLS (mỗi replica một chứng chỉ) |
| Replica xác thực sai sẽ thử lại mỗi giây và ghi log mỗi lần | Chấp nhận: giống standby của PG; lỗi hiện rõ trong log |

## 2a. Đọc-ghi-của-mình qua replica (P6.1, đã cài đặt)

Replica đọc dữ liệu cũ hơn leader một khoảng bằng độ trễ nhân bản. Client vừa ghi rồi đọc ở replica có thể không thấy bản ghi của mình. Cách xử lý: **client mang LSN** của lần ghi cuối, replica chờ tới LSN đó trước khi lấy ảnh chụp. Cùng cách này có ở [PostgreSQL `pg_last_wal_replay_lsn`/`WAIT FOR LSN`](https://www.postgresql.org/docs/current/sql-wait-for.html), [Aurora/RDS `aurora_replica_read_consistency`](https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/aurora-mysql-replication-readcheck.html) và token nhất quán của CockroachDB/Spanner.

Mã: `Engine.WaitDurable` (`internal/core/waitlsn.go`), `internal/sql/minlsn.go`, middleware trong `internal/server/server.go`, bộ điều hướng `internal/router` (lệnh `cmd/staskroute`).

**pgwire / SQL** (biến phiên):
```
SHOW stask.last_lsn;            -- LSN của commit cuối của phiên này (qua leader)
SET stask.min_lsn = 12345;      -- trên phiên ở replica: chờ tới LSN đó trước mỗi lệnh lấy ảnh chụp mới
SET stask.min_lsn_timeout = '2s';  -- mặc định 5s; hết hạn thì lỗi SQLSTATE 55000 (client thử lại hoặc đọc ở leader)
```
Lệnh trong giao dịch tường minh không chờ (ảnh chụp đã lấy ở `BEGIN`); `SET`, `SHOW`, `COMMIT`, `ROLLBACK` không chờ. `last_lsn` cập nhật sau mỗi commit (cả `COPY`), không `SET` được.

**HTTP**: ghi và commit giao dịch trả header `X-Stask-Lsn` (cũng có trong body). Request kèm `X-Stask-Min-Lsn: N` (và tuỳ chọn `X-Stask-Min-Lsn-Timeout: ms`, mặc định 5000) thì node chờ tới N trước khi chạy request; quá hạn trả 503 + `Retry-After`, chưa chạy gì.

**Bộ điều hướng** (`staskroute -leader URL -replicas URL,URL`): ghi, giao dịch, DDL, admin và mọi thứ không biết đều tới leader; `GET /v1/get|history|tables` và `POST /v1/query` tới replica xoay vòng. Router nhớ LSN lớn nhất leader trả về theo phiên (header `X-Stask-Session`, không có thì theo địa chỉ client) và gắn `X-Stask-Min-Lsn` vào đọc. Replica trả 503 hoặc không trả lời thì thử replica kế, hết thì đọc ở leader. Giới hạn: router không tự phát hiện leader đổi (cần P6.2 nối vào); không điều hướng pgwire (phía client dùng `SET stask.min_lsn`); bảng phiên giới hạn 100.000 mục, tràn thì xóa hết (phiên mất token chỉ mất đảm bảo đọc-ghi-của-mình lần kế).

**Đã kiểm chứng** (`internal/router/router_test.go`, có `-race`): `TestReadYourWritesRouter` (300 vòng ghi-rồi-đọc ngay qua router, replica thật qua giao thức nhân bản, mọi lần đọc đều thấy), `TestMinLSNHeader` (chờ đúng, LSN ở tương lai trả 503 sau đúng hạn), `TestSQLMinLSN` (200 vòng INSERT ở leader, `SET stask.min_lsn` ở phiên replica, SELECT luôn thấy; hết hạn thì lỗi). Chưa đo chi phí độ trễ của việc chờ trên tải thật, và chưa thử `psql`/pgx đầu-cuối cho biến phiên (chỉ qua `sql.Session`).

## 3. Kết quả đo giai đoạn A (leader 3 core, replica 1 core, 3 lượt, trung vị)

| Cấu hình | 1x trang p99 | 1x agg p99 | 1,5x trang p99 | 1,5x read/write/tx phục vụ | 1,5x agg p99 |
|---|---|---|---|---|---|
| PG16 | 215 ms | 21,7 s | 1.422 ms | 59/20/31% | 25,6 s |
| PG16 + hot standby | 307 ms | 1,7 s | 1.659 ms | – | 16,1 s (7 lỗi/lượt) |
| STASK, bảng thường | 303 ms | 4,7 s | 561 ms | 56/69/68% | 11,3 s |
| STASK + replica, bảng thường | 159 ms | 23,6 s | 479 ms | 99/85/99% | 24,4 s |
| STASK, bảng `columnar` (2 đợt đo) | 203–213 ms | 0,4–0,5 s | 498–704 ms | 84–100/64–86/92–100% | 0,6–0,9 s |
| **STASK + replica, bảng `columnar`** | **140 ms** | **0,4 s** | **513 ms** | **98/87/99%** | **1,07 s** |

- Với bảng thường, agg p99 trên replica khoảng 24 s là do xếp hàng: 1 core chỉ chạy được khoảng 0,16 agg/s quét dòng (`results/replcost.out`), trong khi áp log chỉ tốn khoảng 30% core (replica áp 163–185 nghìn dòng/s, leader tạo khoảng 54 nghìn dòng/s).
- Với bảng `columnar`, SQL aggregate chạy trên view dạng cột (docs/05 §8), nên 1 core replica là đủ.
- Benchmark chờ view của replica theo kịp leader sau lần nạp 2 triệu dòng (2,7–3,2 s) rồi mới đo. Không chờ thì câu agg đầu tiên quét dòng (8–12 s, `results/clustercol-nowait`).
- Không chờ view theo kịp, nhưng có cơ chế chờ theo tiến độ (docs/05 §8), `results/clustercol-progress`:
  - 1x: agg p99 0,19–0,56 s. Trước khi có cơ chế này là 8–12 s.
  - 1,5x: 2/3 lượt có agg p99 12–17 s. Replica 1 core vừa áp log vừa cập nhật view ở mức 1,5x thì view tụt quá 10 s, nên câu lệnh rơi về quét dòng. Ở tải này cần replica nhiều core hơn, hoặc cho câu lệnh chấp nhận đọc trễ (`stask.read_staleness`).
- Sau khi follower báo tiến độ theo thao tác/thời gian (docs/05 §8), cũng không chờ view sau khi nạp (`results/clustercol-progress`; lần đo trước chuyển sang `-v1`):
  - 1x: agg p99 0,25 s.
  - 1,5x: agg p99 1,6 s (1,1–2,0 s), write phục vụ 84%, tx 100%.
- Replica áp log bằng một lần duyệt B-tree mỗi thao tác (`ReplaceOrInsert` trả về phiên bản cũ) thay vì hai lần (`Get` rồi `ReplaceOrInsert`). Hai lần duyệt đó chiếm 90% thời gian áp log trong profile replica.
- **Đo lại sau thay đổi đó (2026-10-28)**, chỉ dòng replica, cùng cấu hình, không chờ view, 3 lượt (`scripts/clusterrepl.sh`, `results/clustercol-btree`):

  | Tải | Trang p99 | Write p99 | read/write/tx phục vụ | Agg p99 trước (`clustercol-progress`) | **Agg p99 sau** |
  |---|---|---|---|---|---|
  | 1x | 147 ms (144 trước đó) | 62 ms | 100/100/100% | 251 ms (143–1035) | **149 ms** (142–1472) |
  | 1,5x | 567 ms (570 trước đó) | 111 ms | 97/85/98% | 1.608 ms (1.090–1.990) | **226 ms** (156–393) |

  Ở tải 1,5x, agg p99 giảm khoảng 7 lần. Replica 1 core giờ áp log đủ nhanh để view dạng cột không tụt lại, nên câu lệnh không phải rơi về quét dòng.

  Trang p99 và write phục vụ (85%) không đổi, vì cả hai bị giới hạn bởi leader 3 core chứ không phải bởi replica. Lượt 1x có một lần agg p99 1,47 s, xảy ra trong lúc view còn đang theo kịp ngay sau khi nạp.
- **Đo lại với mặc định mới (2026-10-29)**: index không con trỏ (docs/08 §5) và view cột gọn (docs/08 §6). Chỉ chạy các dòng STASK, cùng bố trí core (leader 0–2, replica 3, tải 4–6, probe 7), 3 lượt, trung vị [min–max]. Script `scripts/cluster-rerun.sh`, kết quả `results/cluster-v8/summary.txt`.

  | Cấu hình | Tải | Trang p50 | Trang p99 | Write p99 | read/write/tx phục vụ | Agg p99 |
  |---|---|---|---|---|---|---|
  | STASK, bảng thường | 1x | 107 ms | 272 ms (303 trước) | 44 ms | 84/98/98% | 1,98 s (4,7 s trước) |
  | STASK, bảng thường | 1,5x | 294 ms | 740 ms [583–2.673] (561 trước) | 77 ms | **29/66/46%** (56/69/68% trước) | 2,34 s (11,3 s trước) |
  | STASK + replica, bảng thường | 1x | 5 ms | **91 ms** (159 trước) | 18 ms | 100/100/100% | 23,9 s (23,6 s trước) |
  | STASK + replica, bảng thường | 1,5x | 47 ms | **190 ms** (479 trước) | 34 ms | 98/99/98% (99/85/99% trước) | 25,2 s (24,4 s trước) |
  | STASK, bảng `columnar` | 1x | 5 ms | **100 ms** (203–213 trước) | 18 ms | 100/100/100% | **34 ms** (0,4–0,5 s trước) |
  | STASK, bảng `columnar` | 1,5x | 52 ms | **199 ms** (498–704 trước) | 34 ms | 96/100/100% (84–100/64–86/92–100% trước) | **66 ms** (0,6–0,9 s trước) |
  | STASK + replica, bảng `columnar` | 1x | 5 ms | **93 ms** (140–147 trước) | 20 ms | 100/100/100% | **47 ms** (149 ms trước) |
  | STASK + replica, bảng `columnar` | 1,5x | 50 ms | **203 ms** (513–567 trước) | 38 ms | 100/100/100% (97/85/98% trước) | **62 ms** (226 ms trước) |

  - Mọi cấu hình có replica hoặc bảng `columnar` đều tốt hơn rõ rệt. Trang p99 giảm 1,5–3,5 lần, và ở tải 1,5x thì giờ phục vụ đủ cả ghi lẫn giao dịch. Agg trên bảng `columnar` giờ dưới 70 ms ở cả hai mức tải.
  - Agg trên bảng thường có replica vẫn khoảng 24 s. Nguyên nhân vẫn như cũ: 1 core replica quét dòng không kịp (xem gạch đầu dòng đầu tiên của mục này). Đây là lý do dùng bảng `columnar` cho tải phân tích.
  - **Một dòng xấu đi**: bảng thường, chỉ leader, tải 1,5x. Agg p99 nhanh hơn 5 lần (2,3 s so với 11,3 s), nhưng đổi lại phần read phục vụ được giảm từ 56% xuống 29%, tx từ 68% xuống 46%, và trang p99 tăng từ 561 lên 740 ms (một lượt tới 2,7 s). Ở tải 1x, read phục vụ cũng giảm từ khoảng 100% xuống 84%. Nghĩa là agg quét dòng giờ chạy nhanh hơn nhưng chiếm nhiều CPU hơn của 3 core leader, và lấy CPU đó từ đọc điểm.
  - Nguyên nhân cụ thể **chưa được kiểm chứng**. Giả thuyết: quét dòng không còn bị đẩy xuống ưu tiên thấp như trước, và index mới quét nhanh hơn nên agg tranh CPU dày hơn. Cấu hình này (agg nặng quét dòng ngay trên leader quá tải) vốn không phải cấu hình khuyến nghị; hai cách đã có là bảng `columnar` hoặc replica. Nếu cần giữ đọc điểm trên leader trong cấu hình này thì phải giới hạn CPU cho truy vấn quét (hàng đợi/lane riêng). Việc này để lại, ghi nhận như một điểm yếu.
- Kết quả trong `results/cluster`, `results/clustercol`, `results/clustercol-progress`, `results/clustercol-btree`, `results/cluster-v8`.

## 4. Việc tiếp theo

| Giai đoạn | Nội dung | Tiêu chí đạt |
|---|---|---|
| A (đo) | Agg chạy trên replica, so với PG16 có hot standby, cùng số core | Ở tải cao, trang p99 và đọc trên leader không đổi khi có agg; agg p99 tốt hơn PG |
| A2 (đã làm, §2a) | Đọc ghi-của-mình trên replica (client mang LSN, replica chờ tới LSN đó); bộ điều hướng tự động | Đọc ngay sau ghi qua replica luôn thấy ghi đó |
| B (làm một phần, §5) | Xác nhận ghi theo đa số, Raft failover, lease cho leader | Chaos: giết leader, chia mạng, 0 ghi đã xác nhận bị mất; đo thời gian chuyển leader |
| C | Phân mảnh theo dải key, giao dịch xuyên shard | Sức ghi tăng gần tuyến tính theo số shard |

Giới hạn phần cứng hiện tại: chỉ có 1 VM 8 core. Giai đoạn A được đo bằng cách chia core giữa leader và replica, nên chúng vẫn dùng chung đĩa và bộ nhớ. Kết luận cuối cùng về cách ly cần các máy riêng.

## 5. Giai đoạn B: đồng thuận theo đa số (P6.2 đã nối vào staskd, P6.3 phần lõi)

### 5.1 `internal/raft` (đã cài đặt, kiểm bằng mô phỏng)

Raft kiểu [bài báo](https://raft.github.io/raft.pdf) và [luận án của Ongaro](https://github.com/ongardie/dissertation). Tham khảo thiết kế: [etcd/raft](https://github.com/etcd-io/raft) (máy trạng thái chạy theo tick, không có I/O, nên test điều khiển được bằng thời gian ảo), [hashicorp/raft](https://github.com/hashicorp/raft) (tách Storage và Transport), [lease read của TiKV](https://tikv.org/blog/lease-read/).

Có: bầu leader với **pre-vote** (node bị cô lập không đẩy term lên), nhân bản log và commit theo đa số (chỉ commit entry của term hiện tại, leader mới ghi một entry no-op), trả lời từ chối có gợi ý để lùi nhanh, **check-quorum** (leader mất đa số thì tự xuống), **lease** (`Node.LeaseValid`: đa số đã trả lời tin gửi trong `LeaseTicks`; follower từ chối bầu cho người khác trong `ElectionTicks` sau khi nghe leader, nên không có hai leader cùng có lease), `FileStorage` (log thêm vào cuối, CRC, fsync, cắt đuôi hỏng khi mở lại) và `MemStorage`, `Group` (ticker + `Propose` chờ commit; `ErrLeadershipLost` khi mất quyền trước khi commit, kết quả khi đó là không biết trước).

**Nén log (2026-10-29, nhánh `raftwire`)**: `Node.Compact(index)` bỏ phần đã áp, chỉ giữ `(index, term)` làm gốc (`Snapshot`); `Storage.Compact/Reset`, `FileStorage` ghi lại file (file mới + fsync + rename). Leader thấy follower cần entry đã nén thì gửi `MsgSnap` (mang chỉ số gốc của leader); follower gọi `Config.NeedSnapshot` (nội dung snapshot do bên gọi chuyển, xem §5.2) hoặc, nếu đã vượt gốc đó, trả lời `AppResp` với commit của mình. `TestChaosCompaction` (100 seed): nén ngẫu nhiên ở mọi node và cài snapshot cho node tụt lại (15.436 lần cài), 54.687 entry xác nhận, không mất.

Giao diện tách khỏi core: gói không import gì của STASK. Entry là byte mờ; `Storage`, `Config.Send`, `Config.Apply` do bên gọi cấp.

**Nối vào đường commit**: `core.Quorum` (`internal/core/quorum.go`), một hàm `Replicate(lastLSN, endPos)`. Sau `fdatasync` cục bộ của một batch và trước khi công bố/trả lời, committer gọi nó; nil (mặc định) thì bỏ qua và một node không đổi hành vi hay tốc độ. Lỗi thì engine dừng ghi (như fsync lỗi) và các request của batch nhận `ErrUnknownOutcome`. Test: `TestQuorumHook`, `TestNoQuorumDefault`.

**Đã kiểm chứng** (`internal/raft/sim_test.go`; mô phỏng mạng làm rơi, trễ, nhân đôi, đảo thứ tự tin; chia mạng; giết và khởi động lại node giữ Storage; mọi thứ theo seed):
- `TestChaos`: 150 seed, 3 hoặc 5 node, 3000 bước mỗi seed, giết leader/giết node ngẫu nhiên/chia đôi/cô lập leader/lành lại. Bất biến kiểm mỗi bước: mỗi term một leader; mọi node áp cùng entry tại cùng index; không node nào có lease hợp lệ trong khi node khác là leader term cao hơn. Cuối: lành mạng, phải có leader, mọi node cùng commit index, **mọi entry đã xác nhận cho client (leader áp entry của chính nó ở đúng term) có trong log đã commit**. Lần chạy: 83.892 entry xác nhận, không mất; 343.651 lần kiểm lease.
- `TestFailoverTime`: giết leader của cụm đã ổn định, đếm tick tới khi leader mới commit được một entry (ElectionTicks = 10, nên timeout ngẫu nhiên 10–19 tick): nhỏ nhất 20, trung vị 27, p95 61, lớn nhất 92 tick (100 lượt, 3 và 5 node, không rơi tin). Nhân với độ dài tick: tick 10 ms cho trung vị khoảng 270 ms. **Đây là số trong mô phỏng, không phải đo trên mạng thật.**
- `TestMinorityCannotCommit`: leader bị cô lập không commit, mất lease, tự xuống; đa số bầu leader mới; entry ghi vào leader cô lập không bao giờ được xác nhận.
- `TestSingleNode`, `TestFileStorage` (ghi đè index, đuôi hỏng, khoảng trống bị từ chối).
- Kiểm tra đột biến: sửa quy tắc commit thành "thiếu một phiếu" thì `TestChaos` bắt ngay (hai node áp hai entry khác nhau tại một index). Bỏ việc từ chối bầu trong lease thì **chaos ngẫu nhiên không bắt được** (chaos chỉ cắt cả node, không cắt một chiều một link). Đã bù bằng test nhắm đúng luật đó: `TestLeaseRule` cắt hai chiều link giữa leader và một follower, giữ link của follower còn lại, ngừng ghi để log hai follower bằng nhau (nếu không, ứng viên tụt log bị từ chối vì lý do khác và test không chạm luật), rồi chạy 400 tick: leader phải giữ nguyên, term không tăng, vẫn commit được (60 seed). `TestLeaseRuleMutation` tắt luật (`leaseRuleOff`) và đòi test bắt được: **60/60 seed vi phạm** ("node 3 has a valid lease in term 1 while 1 leads term 2").

@@SEC52@@

### 5.3 Slot, publication, replica identity đi qua log (P6.3, đã cài đặt phần lõi)

Mã: `internal/core/cdcmirror.go`. Trước đây chúng nằm trong file của leader (`slots/`, `cdc.json`) nên replica được nâng lên không có gì. Giờ mỗi thay đổi cũng được ghi vào bảng hệ thống ẩn `__stask_cdc` (như bảng sequence) qua đường commit thường, nên đi tới replica đúng từng byte:
- `catalog`: publication và replica identity, ghi trước khi ghi file.
- `slot:<tên>`: `SlotInfo` (vị trí segment/offset, LSN). Vì log giống từng byte giữa các node nên vị trí có cùng nghĩa trên replica.
- Vị trí slot khi subscriber xác nhận chỉ được ghi vào log tối đa mỗi giây một lần, và không ghi khi đoạn log vừa vượt qua chỉ là chính các ghi này (tránh vòng lặp). Sau failover, vị trí slot ở server có thể tụt tối đa khoảng 1 giây so với điều subscriber đã xác nhận; walsender dùng vị trí subscriber yêu cầu khi `START_REPLICATION`, nên không mất và không trùng (chưa kiểm bằng subscriber PG thật qua failover, xem dưới).
- Lúc mở thư mục không phải replica (khởi động lại không có `-replica-of`, tức nâng cấp), `Open` dựng catalog và slot từ bảng nếu file không có hoặc tụt sau.

Test: `internal/repl/failover_test.go` (`TestCDCStateSurvivesFailover`): leader tạo publication, identity FULL, slot; subscriber nhận 5 giao dịch và xác nhận; ghi thêm 3; replica bắt kịp; leader chết; mở thư mục replica làm leader: publication, identity, slot ở đúng vị trí đã xác nhận, và slot stream đúng 3 giao dịch chưa xác nhận (không mất, không trùng), rồi nhận được ghi mới.

Còn dở: nâng cấp vẫn là khởi động lại thủ công (chưa có promote nóng và chưa gắn với bầu cử của `internal/raft`); chưa chạy `accept/pgsub.sh` (subscriber PG thật) qua failover; replica chạy sống không tự làm mới catalog trong bộ nhớ khi bảng thay đổi (chỉ khi mở lại, đủ cho nâng cấp bằng khởi động lại); slot bị xóa trên leader mà file vẫn nằm trên một node từng là leader sẽ xuất hiện lại; tải ghi thêm: mỗi lần đổi catalog hoặc xác nhận slot (tối đa 1/giây/slot) là một commit.

@@SEC54@@
