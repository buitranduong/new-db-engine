# 33. Sổ tay xử lý sự cố (runbook) và bằng chứng diễn tập G2

Tài liệu này trả lời yêu cầu ở `docs/29_requirements.md` §2: mô hình PostgreSQL + Kafka + ClickHouse trước đây khó vận hành và khó đối chiếu sau sự cố, nên engine phải có **một nguồn sự thật, phát hiện được lệch giữa dữ liệu gốc và các view hay luồng CDC, và tra được nguyên nhân trong phút**. Nó bổ sung cho `docs/21_operations.md` §8 (quy trình ngắn theo từng triệu chứng) bằng: công cụ đối chiếu `staskctl reconcile`, cách tra nguyên nhân theo từng lớp sự cố, và bằng chứng từ các bài diễn tập `scripts/g2-drills` (cổng G2 của `PROCESS.md` D35).

Mọi số trong tài liệu là số của **một lần chạy** trên VM dùng chung (tải nền 6 đến 13, xem dòng "load average" của từng bài); số thời gian khôi phục là **chỉ để tham khảo**, không phải kết luận về hiệu năng (D32). Chỗ nào chưa chứng minh được thì nói rõ ở §7.

| § | Nội dung |
|---|---|
| 1 | Mô hình để tra nguyên nhân: cái gì là nguồn sự thật, cái gì là dẫn xuất |
| 2 | `staskctl reconcile`: đối chiếu core với mọi cấu trúc dẫn xuất và với slot CDC |
| 3 | Bốn câu hỏi đầu tiên (trong 2 phút) |
| 4 | Bảy lớp sự cố: triệu chứng, phát hiện, tra nguyên nhân, sửa, bằng chứng |
| 5 | Bằng chứng: bảng kết quả `scripts/g2-drills` |
| 6 | Phát hiện: lỗi đã sửa và việc tồn đọng kèm tái hiện tối thiểu |
| 7 | Chưa chứng minh |

---

## 1. Mô hình để tra nguyên nhân

- **Core là nguồn sự thật duy nhất**: log (đoạn `wal/*.wal`) và các phiên bản dòng trỏ vào log. Ghi chỉ được xác nhận sau khi log đã `fsync` (cụm: sau khi đa số đã `fsync`).
- **Mọi thứ khác là dẫn xuất và dựng lại được từ core**: view cột (`columnar.view`), view trigram (`tri/`), view tìm kiếm và chuỗi thời gian (trong RAM), kho lịch sử (`histstore/`), chỉ mục phụ (`sidx/` khi để trên đĩa), checkpoint (`ckpt/`). Mỗi view có một *follower* đọc log theo thứ tự và công bố "đã áp tới LSN nào" (`applied`); so với `durable` là biết tụt bao nhiêu bản ghi, bao nhiêu mili giây.
- **CDC** (slot logic) là một người đọc log khác: vị trí đã xác nhận của slot nằm trong `<dir>/slots/<tên>.json`.
- Vì vậy mọi câu hỏi "hai nơi lệch nhau" quy về một phép so sánh: *cấu trúc dẫn xuất tại LSN `A` có bằng core tại LSN `A` không?* Đó chính là việc của `staskctl reconcile`.

## 2. `staskctl reconcile`

```sh
staskctl reconcile -url http://n1:8080[,http://n2:8080,...] [-token T] [-table a,b]
                   [-sample N | -full] [-cdc-slot TÊN] [-retries 3] [-pause-ms 0] [-json]
```

Máy chủ: `POST /v1/admin/reconcile?table=&sample=&slot=&retries=&pause_ms=` (cần credential **đầy đủ** như mọi `POST /v1/admin/*`: không có thì 401, token chỉ đọc thì 403, và được ghi vào audit log như lệnh admin; mỗi lúc chỉ một lượt chạy, lượt thứ hai nhận 409). Mỗi nút được kiểm với **view của chính nó** (trong cụm, chạy trên từng nút).

### 2.1 Nó so sánh gì

Với mỗi bảng và mỗi cấu trúc dẫn xuất của bảng:

| Cấu trúc | `applied` so với `durable` | Số dòng | Nội dung (khi `-sample`) |
|---|---|---|---|
| `columnar` (bảng `columnar = true`) | có | có | các cột view giữ (số nguyên, thời gian, bool, số thực, chuỗi) |
| `trigram(cột)` | có | có | giá trị cột được đánh chỉ mục (không chỉ các trigram) |
| `search`, `timeseries` | có | không | không (chỉ trễ) |
| `index:<tên>` (chỉ mục phụ, RAM hoặc đĩa) | không áp dụng (đồng bộ với commit) | không | mọi dòng thấy được phải có mục trong chỉ mục (mục thừa/cũ là bình thường theo thiết kế) |
| `history` (kho lịch sử, `-hist-store`, chỉ nút đơn) | có | không | phiên bản mới nhất trong kho bằng dòng hiện tại |
| slot (`-cdc-slot`) | LSN đã xác nhận so với `durable` | giao dịch/thao tác chưa giao cho từng bảng | không |

Kết quả mỗi cấu trúc là một trong:

| Trạng thái | Nghĩa | Việc cần làm |
|---|---|---|
| `IN_SYNC` | đã áp tới `durable` và bằng core | không |
| `BEHIND` | `applied < durable`: in "tụt X bản ghi / Y ms"; nếu có `-sample`, nội dung được so ở đúng LSN `applied` của nó nên *tụt nhưng đúng* khác *tụt và sai* | §4.5 |
| `DIVERGED` | khác core tại cùng LSN; in khoá đầu, khoá cuối bị lệch, số khoá thiếu/thừa/đổi, tối đa 20 ví dụ | §4.5 |
| `SUSPECT` | khác trong mọi lượt trong khi cấu trúc vẫn đang chạy nên không lượt nào chứng minh được; in các khoá | chạy lại khi tải thấp |
| `INCONCLUSIVE` | không quyết được (ALTER TABLE đang chạy, ghi quá nhanh) | chạy lại |
| `NOT_READY` | view đang dựng lại sau khởi động | đợi `/v1/ready` |
| `UNCHECKED` | chỉ so độ trễ | |

Mã thoát: 0 mọi thứ khớp; 3 tụt hoặc chưa quyết được; 4 lệch (`DIVERGED`, `SUSPECT`, slot `INVALID`); 1 công cụ lỗi.

### 2.2 Cách so nội dung và chi phí

- `-sample 0` (mặc định): chỉ LSN và số dòng, độ phức tạp O(1) mỗi cấu trúc: rẻ, dùng được từ giám sát định kỳ trên hệ đang rảnh. Ví dụ đo thực: 2000 dòng, 0 ms.
- `-sample N`: mỗi dòng cho vào một tổng kiểm 64 bit (băm khoá + băm các cột cấu trúc giữ), chia vào 1024 xô theo băm khoá; chỉ các khoá có băm chia hết cho `N` được tính (`-full` = `-sample 1`). Tổng kiểm **không phụ thuộc thứ tự**, nên hai bên đi theo thứ tự riêng của mình. Xô nào lệch thì đọc lại đúng các xô đó để tìm khoá lệch; chỉ khẳng định `DIVERGED` khi cấu trúc đứng ở đúng LSN lúc so (cặp `applied` đọc trước và sau lượt duyệt bằng nhau và bằng LSN đọc core), nếu không thì lặp tối đa `-retries` lần và đưa phần giao của các lượt (`SUSPECT`).
- Mỗi lượt đọc **theo khúc** (2048 dòng core, 4096 dòng view mỗi lần). Khoá bảng của view (cột: khoá `RWMutex` của bảng) chỉ giữ trong một khúc, không bao giờ cả lượt duyệt; core đọc trên bản chụp copy-on-write. `-pause-ms` nhường chỗ cho tải thật. Không có phép đo hiệu năng nào trong tài liệu này; chi phí là một lượt đọc tuyến tính toàn bảng cho mỗi cấu trúc được so (core đọc hai lần nếu có hai view), nên với bảng lớn dùng `-sample 64` hay chạy ngoài giờ.
- Bảng có khoá chính **không phải số nguyên** ở view cột: view không liệt kê được khoá, nên chỉ có tổng kiểm cả bảng (không chỉ ra được khoảng khoá lệch).
- Đã kiểm trong diễn tập: sau mỗi chu kỳ `kill -9`, mọi cấu trúc `IN_SYNC` (D1, D2, D6, D3); lật một byte trong kho lịch sử làm `history` lệch 16 khoá trong khi engine không báo gì (D4, lỗi F2 ở §6); `-cdc-slot` báo `INVALID` cho slot bị vô hiệu và `BEHIND` cho consumer chưa xác nhận (D7); test đơn vị `internal/reconcile` cố ý chèn dòng đổi/thừa/thiếu vào view và kiểm khoảng khoá in ra.

### 2.3 Ví dụ

```text
$ staskctl reconcile -url http://127.0.0.1:8080 -sample 1
== http://127.0.0.1:8080  durable_lsn=3  verdict=IN_SYNC  (9 ms, sample all keys)
table acct  core_rows=2000
  columnar                     IN_SYNC      applied=3 durable=3  rows core=2000 view=2000
      checked: all keys at lsn 3 (2000 keys)
  trigram(note)                IN_SYNC      applied=3 durable=3  rows core=2000 view=2000
      checked: all keys at lsn 3 (2000 keys)
```

Khi lệch (trích kết quả D4, lật một byte trong `histstore/open-*.log` trước khi sửa F2):

```text
acct/history=DIVERGED(16 keys "0".."9")   ->   first differing key "0", last "9", 16 keys changed
```

---

## 3. Bốn câu hỏi đầu tiên (2 phút)

1. **Nút còn sống và nhận ghi không?** `curl -s :8080/v1/health` (engine không lỗi ghi), `curl -si ':8080/v1/ready?write=1'` (200 chỉ ở nút nhận ghi), `staskctl status -url n1,n2,n3` (vai trò, term, `commit`/`applied`). Lưu ý: lúc từ chối ghi vì hết chỗ cấp segment mới, `/v1/health` vẫn 200 (F7).
2. **Nhật ký nói gì?** `grep -E 'FATAL|panic:|cannot allocate|torn_tail=true|not used|rebuilding|crc|invalidated|bad record|failed' staskd.log | tail`. Các dòng đã gặp trong diễn tập có ở §4.
3. **Đĩa và giữ log?** `df -h`; `stask_wal_disk_bytes`; `stask_cdc_slot_lag_bytes{slot}`; `stask_replication_lag_bytes`.
4. **View có khớp core không?** `staskctl reconcile -url ...` (mặc định rẻ), rồi `-sample 64` hoặc `-full` nếu cần nội dung. Kèm `POST /v1/admin/scrub?wait=1&rate_mb=0` nếu nghi hỏng đĩa (trả 500 và liệt kê đoạn hỏng).

Thang chẩn đoán theo kết quả: nút không trả lời -> §4.1; hai nút cùng nhận ghi hoặc không ai nhận -> §4.2; lỗi ghi "no space" -> §4.3; `scrub` 500 hoặc dữ liệu khác nhau giữa các nút -> §4.4; `reconcile` `BEHIND`/`DIVERGED` -> §4.5; slot `INVALID`/lag tăng -> §4.6; cần dựng lại -> §4.7.

---

## 4. Bảy lớp sự cố

### 4.1 Nút chết (process mất, `kill -9`, máy khởi động lại)

- **Triệu chứng**: `stask_up == 0`, cổng không trả lời, client nhận lỗi kết nối; trong cụm các nút khác bầu leader mới trong khoảng 1 đến 3 s.
- **Phát hiện**: `staskctl status` (nút không `ready`), `pg_is_in_recovery()`, nhật ký của nút cuối cùng (`tail -50`).
- **Tra nguyên nhân**: dòng cuối nhật ký trước khi chết: `panic:` kèm tên tệp:dòng (tiến trình tự dừng, xem §4.3 nếu là `storage failure`), OOM (`dmesg | grep -i kill`), hay không có gì (bị `kill -9`/mất điện). Sau khi khởi động lại, dòng `recovery: lsn=... replay_records=... torn_tail=...` cho biết đã phát lại bao nhiêu và có cắt đuôi rách không.
- **Sửa**: khởi động lại **cùng thư mục, cùng cờ** (nút đơn: recovery phát lại từ checkpoint; cụm: nút nhận phần còn thiếu từ leader, hoặc snapshot nếu tụt quá `-raft-compact-keep`). Không xoá thư mục dữ liệu. Sau đó `staskctl reconcile`: view dựng lại hay nạp từ `columnar.view` đều phải `IN_SYNC` khi đã bắt kịp.
- **Bằng chứng**: D1 nút đơn: 10 chu kỳ `kill -9` ở thời điểm ngẫu nhiên (400 ms đến 6 s sau khi bắt đầu tải), cứ chu kỳ thứ tư lại giết thêm lần nữa *trong lúc đang recovery*; sau mỗi chu kỳ, oracle: mọi giao dịch đã xác nhận có đúng một lần (37 221 xác nhận; 23 không rõ kết quả, trong đó 13 đã commit và 10 chưa, mỗi cái có cả hai dòng hoặc không dòng nào, không giao dịch dở), bất biến số dư giữ nguyên, 5 313 giao dịch rollback không để lại dòng nào; sau cùng cột/trigram/chỉ mục/lịch sử bằng core. D1 cụm: giết cả 3 nút cùng lúc, 10 chu kỳ: 8 304 xác nhận, 0 mất. D2: giết leader rồi follower 6 lần dưới tải, nút quay lại bắt kịp: 19 801 xác nhận, 0 mất, ba nút giống nhau và đúng, leader mới sau 1,3 đến 3,1 s (VM tải 9 đến 13).

### 4.2 Đổi leader, leader bị đóng băng, hai leader

- **Triệu chứng**: ghi lỗi `25006` (nút không phải leader) hoặc treo vài giây; `sum(stask_raft_is_leader) != 1`; `stask_raft_term` tăng.
- **Phát hiện**: `staskctl status` (cột leader/term), `GET /v1/cluster` từng nút (`role`, `lease`, `writable`, `commit`, `applied`), `/v1/ready?write=1`.
- **Tra nguyên nhân**: (a) leader chết: §4.1. (b) leader còn tiến trình nhưng đứng (SIGSTOP, đĩa treo, GC dài, mạng): nút khác bầu leader sau thời gian bầu cử (`-raft-tick` × `-raft-election-ticks` đến gấp đôi, mặc định 1 đến 2 s); nhật ký `cluster: node N is the leader of term T`. (c) hai nút cùng báo ghi được: chỉ chấp nhận trong vài chục mili giây sau khi nút cũ tỉnh lại (F4); nếu kéo dài, kiểm đồng hồ và mạng giữa các nút.
- **Sửa**: thường không cần làm gì. Client phải có timeout theo từng host (F6): DSN nhiều host của pgx/libpq với `connect_timeout` ngắn có thể thất bại cả danh sách khi host đầu bị đóng băng; `socketTimeout`/`TCP_USER_TIMEOUT` để giao dịch đang treo trên leader cũ tự bỏ. Giao dịch bị cắt giữa chừng có kết quả **không rõ**: client phải kiểm lại (khoá tự nhiên hoặc `ev`-kiểu bảng ghi nhận) chứ không ghi lại mù quáng.
- **Bằng chứng**: D6: đóng băng leader 10 s (dài hơn thời gian bầu cử), nút khác lên leader sau 1,63 s và 1 811 giao dịch được xác nhận trong lúc đóng băng; sau SIGCONT, nút cũ vẫn trả "nhận ghi" cho `/v1/ready?write=1` trong 10 ms (lần chạy khác: 35 ms; F4) nhưng 177 ghi gửi **thẳng** vào nó bị từ chối, 0 được xác nhận; không giao dịch xác nhận nào mất (4 104 xác nhận), 3 nút giống nhau.

### 4.3 Đĩa đầy

- **Triệu chứng**: ghi lỗi `XX000 cannot allocate log segment (disk full?): preallocate segment: no space left on device`; `df` 100%; trong cụm một nút tự dừng.
- **Phát hiện**: lỗi ghi ở client và nhật ký (`commit: cannot allocate log segment`); `stask_wal_disk_bytes`, `df -h`. **Không dựa vào `/v1/health`**: nó vẫn 200 vì từ chối ghi ở bước cấp segment là lỗi sạch, không phải engine hỏng (F7); cảnh báo phải bắt `df`/`stask_wal_disk_bytes` > 70% (docs/21 §5.3) và tỉ lệ lỗi ghi.
- **Tra nguyên nhân (ai giữ chỗ)**: `du -sh <dir>/*` (wal, histstore, sidx, tri, ckpt, spill), `stask_cdc_slot_lag_bytes` (slot CDC bỏ quên giữ log), `stask_replication_lag_bytes` (replica tụt), `-archive-dir` đầy, session `BEGIN` bỏ quên ghim snapshot (docs/12 M3), GC tắt (`-gc=false` là mặc định trong cụm).
- **Sửa**: giải phóng chỗ (xoá slot CDC không dùng `pg_drop_replication_slot`, đặt `-slot-max-lag-mb`, dọn thứ ngoài engine). Nút đơn: ghi tự tiếp tục khi có chỗ, **không cần khởi động lại**. Cụm: nút đã tự dừng (fail-stop) thì khởi động lại với cùng thư mục sau khi có chỗ; nó nhận phần còn thiếu từ leader. Sau đó `scrub` và `reconcile`.
- **Hành vi khác nhau theo nút**: nút đơn hoặc leader còn sống từ chối ghi sạch; follower hết chỗ **tự dừng** với `panic: raft N: storage failure, the node cannot continue: write .../raft/raft.log: no space left on device` (hoặc `cluster: FATAL applying entry ...: preallocate segment: no space left on device`) và đa số vẫn ghi được; **leader hết chỗ**: trong diễn tập hầu hết lần nó tự dừng và đa số bầu leader mới sau ~3 s, nhưng đã gặp một lần nó **còn sống và chặn ghi toàn cụm** (nhật ký lặp `commit: cannot allocate log segment`) cho tới khi người vận hành dừng nó (F5): nếu cụm không nhận ghi mà cả hai nút còn lại khoẻ, kiểm đĩa của leader trước.
- **Bằng chứng**: D3 nút đơn (đĩa 96 MB, "chặn đệm" 58 MB): sau 4 s và 13 684 dòng 3 KB đã xác nhận, ghi bị từ chối với lỗi nói rõ lý do (SQLSTATE XX000, `cannot allocate log segment (disk full?): preallocate segment: no space left on device`); 4 s sau không có ghi nào được xác nhận; `/v1/health` vẫn 200 (F7); đọc vẫn phục vụ (13 684 dòng thấy được); `kill -9` rồi khởi động lại trên đĩa đầy chạy được (và còn nhận một ghi nhỏ vì phần còn trống của đoạn hiện tại); xoá chặn đệm thì ghi nhận lại **không cần khởi động lại**; mọi ghi đã xác nhận có đủ (2 978 giao dịch bank, 21 786 dòng fill), không dòng ma, scrub sạch, view bằng core. D3 cụm (một nút trên đĩa nhỏ, hai trường hợp follower/leader): follower dừng với thông điệp trên, đa số xác nhận tiếp 634 giao dịch bank và 857 dòng fill trong 4 s; khi leader hết chỗ nó tự dừng (`panic: raft 3: storage failure ...`), leader mới nhận ghi sau 3 s; cả hai nút hồi phục sau khi có chỗ (có lần tự lành không cần khởi động lại, có lần phải khởi động lại) và bắt kịp, mọi ghi xác nhận có trên cả 3 nút (4 423 giao dịch bank, 7 405 dòng fill).

### 4.4 Dữ liệu hỏng (bit rot, ghi dở, sửa tay)

- **Triệu chứng**: lúc khởi động `open: load checkpoint ...: checkpoint chunk crc mismatch` (từ chối khởi động); `columnar: saved view not used: ... checksum mismatch: rebuilding` (tự dựng lại); `recovery: ... torn_tail=true`; `POST /v1/admin/scrub?wait=1` trả 500 với `errors` liệt kê đoạn/offset; `stask_scrub_errors_total` > 0; hoặc dữ liệu khác nhau giữa các nút. **Hỏng trong đoạn log đã niêm phong KHÔNG được báo lúc khởi động hay lúc đọc (F1)**: đọc có thể trả byte sai cho tới khi scrub chạy.
- **Phát hiện**: bật `-scrub-interval 6h -scrub-rate-mb 32` trên mọi nút production và cảnh báo `increase(stask_scrub_errors_total[1d]) > 0` (đã có ở docs/21 §5.3); `POST /v1/admin/scrub?wait=1&rate_mb=0` sau mọi sự cố lạ; trong cụm so tổng kiểm hai nút (`SELECT *` băm ở client, như D4) hoặc `staskctl reconcile` từng nút.
- **Tra nguyên nhân**: tệp nào hỏng (scrub in `segment N: bad record (CRC or framing) at offset O`); kiểu hỏng quyết định cách sửa:

| Tệp hỏng | Engine làm gì (đã đo ở D4) | Sửa |
|---|---|---|
| `ckpt/ckpt-*.ckpt` | từ chối khởi động, nói `checkpoint chunk crc mismatch` | đặt checkpoint hỏng sang bên (nó chỉ là tăng tốc) rồi khởi động: recovery phát lại log từ đoạn cũ hơn; nếu log đã bị GC thì khôi phục (§4.7) |
| `columnar.view` | bỏ qua, ghi `saved view not used: section checksum mismatch: rebuilding`, dựng lại từ core; dữ liệu và view đúng | không cần |
| `sidx/*.pidx` (chỉ mục phụ trên đĩa) | dựng lại; dữ liệu, chỉ mục đúng (`reconcile`: `index:<tên>` `IN_SYNC`) | không cần |
| `histstore/open-*.log` (giữa tệp) | **từ F2 (đã sửa)**: ghi `bad record at offset ... and intact records after it`, giữ bản sao phần cắt `.damaged-<giờ>`, bỏ vị trí đã lưu, đánh dấu khoảng trống trong `cover.json` (AS OF trong khoảng đó bị từ chối chứ không đoán); trước khi sửa, nó **cắt im lặng** và lịch sử mất. Phần lịch sử của xô bị hỏng **chưa** được đọc lại từ log (F2b): `reconcile` vẫn báo `history` `DIVERGED` cho tới khi dựng lại | xem log; dựng lại từ bản sao lưu bằng `-hist-store-backfill-dir` |
| `wal/*.wal` đã niêm phong, được checkpoint che | **không phát hiện lúc khởi động**; đọc trả byte sai (F1); scrub trả 500 | khôi phục: nút đơn từ bản sao lưu + PITR (§4.7); cụm: xoá thư mục nút đó và để nó nạp lại từ leader (D4 cụm) |
| `wal/*.wal` đuôi (đoạn mới nhất, sau checkpoint) | frame sai CRC ở cuối coi như đuôi rách: cắt, ghi `torn_tail=true`; ghi nhận sau điểm hỏng bị mất (không bảo đảm của nút đơn, docs/01) | cụm: nút nhận lại từ leader |

- **Sửa**: theo bảng. Nguyên tắc: không bao giờ "sửa tay" log; nút nghi hỏng ở trong cụm thì dựng lại từ leader, nút đơn thì từ bản sao lưu kiểm thử gần nhất.
- **Bằng chứng**: D4 (một byte đảo bit ở giữa tệp, mỗi ca một bản sao thư mục dữ liệu nguyên vẹn): xem §5; D4 cụm: follower có byte hỏng trong đoạn log cũ khởi động không báo gì và `SELECT *` bảng `fill` khác leader (15 567 dòng nhưng băm khác); `scrub` trả 500; dựng lại nút theo quy trình có tài liệu (`staskctl cluster remove`, xoá thư mục, khởi động với `-raft-join`, `staskctl cluster add`, docs/06 §5.2b) thì nó nạp lại và băm bằng leader. **Chỉ xoá thư mục rồi khởi động lại không đủ**: trong diễn tập nút đó ở lại rỗng, không nhận gì từ leader trong 180 s (F10).

### 4.5 View tụt hoặc lệch

- **Triệu chứng**: truy vấn chậm bất thường (planner rơi về đọc core), `stask_view_ready == 0`, `stask_view_lag_seconds > 5`, aggregate từ view khác kết quả đọc dòng, `/v1/ready` 503.
- **Phát hiện**: `staskctl reconcile` (xem `BEHIND` hay `DIVERGED`), `stask_view_lag_seconds{view}`.
- **Tra nguyên nhân (vài phút)**:
  1. `BEHIND` với `applied` tăng dần: view đang theo kịp; do tải ghi lớn hay do view bị khoá lâu (truy vấn nặng). Đợi hoặc giảm tải; nếu `applied` đứng yên: xem nhật ký dòng `log read failed` hoặc `bootstrap failed` của view đó (follower dừng ở bản ghi không đọc được).
  2. `BEHIND` mà nội dung ở LSN `applied` **bằng** core (chạy với `-sample 1`): view chậm nhưng đúng; không cần sửa dữ liệu.
  3. `DIVERGED` có khoảng khoá: lấy `first_key` và `last_key`, `SELECT` các khoá đó trong `BEGIN ... COMMIT` (đọc dòng, core) và ngoài giao dịch (có thể qua view) để thấy hai giá trị; so `LSN` ở bản ghi nguồn bằng `stasklogdump`. `missing_in_view` nghĩa là view bỏ lỡ cập nhật, `extra_in_view` là view giữ dòng đã xoá, `changed` là giá trị khác.
- **Sửa**: view dựng lại được từ core, nên cách an toàn: khởi động lại nút với `-no-view-file` (bỏ `columnar.view`) để dựng lại view cột từ core; trigram/search luôn dựng lại khi khởi động; kho lịch sử: xem bảng ở §4.4. Sau đó chạy lại `reconcile -sample 1`. Nếu view lệch lặp lại sau khi dựng lại thì đó là lỗi engine: giữ nguyên nút (hoặc `cp -a` thư mục dữ liệu), ghi `first_key..last_key` và LSN, báo lỗi.
- **Bằng chứng**: tất cả bài `kill -9` kết thúc bằng `reconcile -full` trên mọi nút: `IN_SYNC` ở cả 5 cấu trúc (cột, trigram, hai chỉ mục phụ, và `history` ở nút đơn). D4: `columnar.view` hỏng được dựng lại, kho lịch sử hỏng được `reconcile` bắt (trước F2). Test đơn vị (`internal/reconcile`) giữ một view bằng `Hold()` để thấy `BEHIND` mà *không* `DIVERGED`, và chèn bản ghi giả vào view để thấy `DIVERGED` với đúng khoảng khoá.

### 4.6 Consumer CDC kẹt hoặc chậm

- **Triệu chứng**: `stask_cdc_slot_lag_bytes{slot}` tăng; `stask_wal_disk_bytes` tăng; sau ngưỡng `-slot-max-lag-mb` slot bị **vô hiệu** (nhật ký `cdc: slot X invalidated: fell N MiB behind (limit M MiB)`), consumer nhận lỗi `replication slot was invalidated` (HTTP 410).
- **Phát hiện**: `staskctl reconcile -cdc-slot TÊN [-table T]`: in LSN đã xác nhận, `durable`, tụt bao nhiêu bản ghi/byte, và **với từng bảng**: bao nhiêu giao dịch/thao tác chưa giao, cái cũ nhất cách đây bao lâu; `BEHIND` nếu còn thay đổi chưa giao, `IN_SYNC` nếu slot chỉ tụt vì bản ghi của bảng khác, `INVALID`/`LOG_MISSING` nếu mất. Slot dùng chung cho nhiều bảng nên "tụt" chung chung chưa nói gì: cột theo bảng mới là câu trả lời.
- **Tra nguyên nhân**: consumer chết hay đang kẹt: `pg_stat_replication`/`pg_replication_slots` (`active`, `confirmed_flush_lsn`), nhật ký của consumer; nó không `ack`/gửi `standby_status_update`? tụt đều (chậm hơn tốc độ ghi) hay đứng (kẹt)? so `confirmed_lsn` hai lần cách 30 s.
- **Sửa**: consumer sống lại thì tự đuổi kịp nếu slot còn hợp lệ. Slot `INVALID`: consumer phải đồng bộ lại từ bản chụp (dump + slot mới), vì thay đổi trước thời điểm vô hiệu đã mất; xoá slot cũ `pg_drop_replication_slot`. Đặt `-slot-max-lag-mb` ở mọi nơi có consumer ngoài (bảo vệ đĩa).
- **Bằng chứng**: D7 (`-slot-max-lag-mb 12`, đoạn log 4 MB, ghi ghi đè liên tục ~1 KB mỗi cập nhật): slot không ai đọc (`stalled`) tụt tăng đơn điệu tới 27,1 MB rồi bị vô hiệu sau 16 s; slot đọc chậm (`slow`, xác nhận mỗi 25 ms) bị vô hiệu cũng sau 16 s; slot khoẻ (`healthy`, xác nhận theo lô) không bị đụng tới, lag cuối 0,03 MB; log trên đĩa không vượt 28 MB (không tăng vô hạn); 32 670 ghi, 0 lỗi; đọc slot bị vô hiệu trả 410 (sau khi sửa F8, trước đó 200 kèm một dòng lỗi); `reconcile -cdc-slot` báo `INVALID` cho slot hỏng và `BEHIND` 3 bản ghi cho slot khoẻ đang đuổi. Kịch bản đối chiếu nội dung CDC với bảng (consumer áp lại bằng bản sao) là `scripts/suite-cdc` d_oracle (docs/26).

### 4.7 Cần khôi phục (hỏng không sửa được, nhầm lệnh, mất đĩa)

- **Quyết định**: dữ liệu *còn đúng nhưng cần quay lại thời điểm trước* (xoá nhầm) -> PITR; *đĩa/nút mất* trong cụm -> nút mới nạp từ leader, hoặc khôi phục cụm từ sao lưu cụm; *nút đơn mất* -> bản sao lưu + đoạn lưu trữ.
- **Quy trình PITR nút đơn** (`accept/pitr.sh`, D5): `staskctl restore -backup DIR/<giờ> -dir MỚI` (chép bản sao lưu), rồi `staskd -dir MỚI -segment-mb N -restore-archive KHO -recovery-target-time <RFC3339>` hoặc `-recovery-target-lsn <LSN>` (N in sẵn lúc restore; `-archive-key-file` nếu kho mã hoá). Mốc: ghi lại **thời điểm và LSN** (`durable_lsn` của `/v1/status`) *trước* thao tác nguy hiểm; "stop before the first commit after T": T phải sau commit cuối muốn giữ. Đoạn lưu trữ chỉ chứa đoạn log **đã niêm phong**: phần đang ghi dở ở đoạn cuối không có trong kho (phát lại không mục tiêu dừng ở cuối đoạn niêm phong cuối).
- **Kiểm sau khôi phục**: số dòng và tổng kiểm so với bản ghi lúc sao lưu (bài D5 dùng băm mọi dòng); `staskctl reconcile -full` trên nút mới; `POST /v1/admin/scrub?wait=1`; thử ghi.
- **Bản sao lưu phải kiểm định kỳ**: `staskctl verify -backup DIR` là một lần khôi phục thật (manifest, khởi động, scrub, đếm dòng). `backup-loop` làm sẵn lịch và file Prometheus.
- **Mã hoá**: `staskctl keygen -out F`; `backup -key-file F`, `restore -key-file F`, `verify -key-file F`. Sai khoá hay thiếu khoá bị từ chối ngay ở bước kiểm khoá và không để lại tệp. Khoá không bao giờ nằm trong bản sao lưu: mất khoá = mất bản sao lưu.
- **Bằng chứng (D5, 5 000 000 dòng, bảng `big` có view cột)**: bản sao lưu thường 246 MB, `verify` pass (manifest, khởi động, scrub, 5 000 000 dòng), khôi phục vào thư mục mới, nút phục vụ lại, **số dòng và tổng kiểm của từng dòng bằng nguồn**, view `IN_SYNC`. Bản sao lưu mã hoá AES-256-GCM: không chứa văn bản gốc (grep một giá trị giữa bảng), `verify` không khoá bị từ chối, `restore` khoá sai bị từ chối (`wrong encryption key: the backup was made with key id ..., this key is ...`), khôi phục bằng khoá đúng bằng nguồn. PITR: sau bản sao lưu, 180 giao dịch đổi 270 000 dòng; ghi thời điểm `T` và LSN `L`, băm toàn bảng; nguồn tiếp tục ghi tới khi kho có thêm 3 đoạn; khôi phục theo thời gian `T` và theo LSN `L` cho ra đúng bảng đã băm (5 080 000 dòng), khôi phục theo một LSN ở giữa đợt ghi sau cho ra đúng tổng hợp đã ghi, phát lại không mục tiêu dừng ở cuối đoạn niêm phong; mọi nút khôi phục `IN_SYNC` và nhận ghi. Thời gian (chỉ để tham khảo, tải nền 10 đến 13, VM dùng chung, không phải thước đo): sao lưu 3 s, khôi phục (chép) 1 s, nút phục vụ sau khoảng 1 s (view nạp từ tệp đã lưu), PITR phục vụ sau 1 đến 4 s; dữ liệu đĩa chỉ 192 MB cho 5 triệu dòng nên thời gian này **không ngoại suy** sang 30 đến 50 triệu dòng hay sang dữ liệu thật.

---

## 5. Bằng chứng: kết quả `scripts/g2-drills`

Chạy: `scripts/g2-drills/run.sh [-drills D1,...] [-modes single|cluster] [-cycles N] [-rows N] [-keep]`. Mỗi bài tự dựng tiến trình `staskd` trong thư mục tạm riêng, cổng 21000 đến 21999 khoá bằng `flock` ở `/tmp/stask-test-ports` (như bộ test), chỉ gửi tín hiệu cho tiến trình mình đã bật, dọn hết khi xong, in `PASS`/`FAIL` theo oracle. Bài D5 giữ `/tmp/stask-bigdata.lock`. Hạt giống ngẫu nhiên in ra để lặp lại điểm giết.

Oracle chung của bài tải (`bank.go`): mỗi giao dịch `(writer, seq)` cập nhật hai tài khoản (số dư, phiên bản, ghi chú) và chèn hai dòng `ev` (ghi nợ/ghi có), một phần giao dịch rollback sau khi đã ghi; client giữ sổ: *đã xác nhận*, *không rõ kết quả* (kết nối đứt khi COMMIT đang bay), *rollback*. Kiểm: mọi giao dịch xác nhận có **đúng hai dòng `ev` đúng nội dung** (đúng một lần); giao dịch không rõ kết quả có 0 hoặc 2 dòng (không dở dang); giao dịch rollback và `(writer, seq)` chưa từng gửi không có dòng nào; với từng tài khoản `bal = 1000 + ghi có - ghi nợ` và `ver = số chân` suy ra từ chính các dòng `ev` đã commit; tổng số dư không đổi; sau đó `reconcile -full` mọi nút `IN_SYNC` và aggregate từ view cột bằng aggregate đọc dòng.

Kết quả (2026-10-31, VM 8 vCPU dùng chung, load average lúc bắt đầu từ 2,6 đến 13; mỗi dòng là MỘT lần chạy; số liệu chính xác nằm trong `results/g2-drills/report.txt` do `run.sh` ghi):

| Bài | Chế độ | Kết quả | Oracle đã kiểm | Số liệu của lần chạy |
|---|---|---|---|---|
| D1 `kill -9` lặp | nút đơn (có kho lịch sử, chỉ mục đĩa) | **PASS** | sổ giao dịch bank, bất biến, `reconcile -full`, scrub, aggregate view = đọc dòng | 10 chu kỳ, kill sau 0,4 đến 6 s, cứ chu kỳ thứ 4 giết thêm lần nữa lúc recovery; 37 221 xác nhận, 23 không rõ (13 có mặt), 5 313 rollback; 7 cấu trúc `IN_SYNC` |
| D1 | cụm 3 nút (giết cả ba cùng lúc) | **PASS** | như trên, trên cả ba nút | 8 304 xác nhận, 31 không rõ (11 có mặt), 1 155 rollback; 5 cấu trúc `IN_SYNC` mỗi nút |
| D2 giết leader và follower rồi cho vào lại | cụm | **PASS** | như trên + ba nút giống nhau | 6 lần giết leader (leader mới sau 1,3 đến 3,1 s), 6 lần giết follower; 19 801 xác nhận, 0 mất, 15 không rõ (0 có mặt), 2 829 rollback |
| D3 đĩa đầy | nút đơn (tmpfs 96 MB) | **PASS** | từ chối ghi có lý do, đọc phục vụ, khởi động lại khi đầy, sổ fill + bank, scrub, `reconcile -full` | từ chối sau 4 s/13 684 dòng 3 KB; ghi nhận lại khi có chỗ không cần khởi động lại; 2 978 bank + 21 786 fill xác nhận, đủ |
| D3 | cụm (một nút trên đĩa nhỏ: ca follower rồi ca leader) | **PASS** | đa số tiếp tục ghi; nút đầy tự dừng và vào lại; ba nút giống nhau | follower: 634 bank + 857 fill xác nhận trong 4 s sau khi nó dừng; leader tự dừng, leader mới sau 3 s; 4 423 bank + 7 405 fill xác nhận, đủ trên 3 nút. Ghi chú F5: một lần chạy khác leader còn sống chặn ghi |
| D4 hỏng dữ liệu | nút đơn, 7 ca | **PASS** (có 4 phát hiện) | mỗi ca: REFUSED + thông điệp, hoặc REPAIRED với dữ liệu = bản nguyên vẹn và view `IN_SYNC`, hoặc ghi nhận phát hiện | `ckpt` hỏng: REFUSED (`checkpoint chunk crc mismatch`), chuyển checkpoint sang bên rồi phát lại toàn bộ log: REPAIRED; `columnar.view` hỏng: REPAIRED (`saved view not used ... rebuilding`); `sidx` hỏng: REPAIRED (`index ev_acct_idx: scan of the disk file failed (... corrupt page ...): rebuilding`); `histstore` hỏng giữa tệp: DAMAGE_REPORTED (F2 đã sửa, còn F2b); ba ca log đã niêm phong/đuôi: dữ liệu `SELECT *` khác bản gốc, khởi động không một lời, chỉ `scrub` trả 500: **F1** |
| D4 | cụm, follower có đoạn log cũ hỏng | **PASS** | follower khởi động im lặng (F1) và khác leader; dựng lại theo quy trình có tài liệu thì bằng leader | `SELECT *` bảng `fill` khác leader, scrub 500; `cluster remove` + xoá + `-raft-join` + `cluster add` -> bằng leader |
| D5 sao lưu/khôi phục/PITR | nút đơn, 5 000 000 dòng, bảng có view cột | **PASS** | số dòng và tổng kiểm từng dòng bằng nguồn tại mốc; `verify`; không có văn bản gốc trong bản mã hoá; sai khoá bị từ chối; view `IN_SYNC`; nút khôi phục nhận ghi | bản sao lưu 246 MB 3 s, `verify` 11 s, khôi phục (chép) 1 s rồi nút phục vụ sau ~0,7 s; mã hoá 3 s, khôi phục 2 s/0,8 s; PITR theo thời gian và theo LSN 202 -> 5 080 000 dòng, tổng kiểm đúng; theo LSN 852 giữa đợt ghi sau -> tổng hợp đúng; không mục tiêu -> dừng ở LSN 1 371/1 437 (đoạn cuối chưa niêm phong không có trong kho). **Chỉ để tham khảo**, tải nền 5 đến 14 |
| D6 SIGSTOP leader | cụm | **PASS** (phát hiện F4) | không ghi xác nhận nào mất; một leader sau khi tỉnh; ghi gửi thẳng vào nút tỉnh lại không được xác nhận | đóng băng 10 s; leader mới sau 1,63 s; 1 811 giao dịch xác nhận trong lúc đóng băng; nút tỉnh lại còn trả "nhận ghi" 10 ms; 177 ghi trực tiếp bị từ chối; 4 104 xác nhận |
| D7 consumer CDC chậm/kẹt | nút đơn (`-slot-max-lag-mb 12`) | **PASS** | lag slot tăng đơn điệu rồi slot bị vô hiệu; log bị chặn; slot khoẻ nguyên vẹn; ghi không lỗi; 410; `reconcile -cdc-slot` | slot kẹt đạt 27,1 MB rồi bị vô hiệu sau 16 s; slot chậm cũng sau 16 s; log tối đa 28 MB; 32 670 ghi, 0 lỗi |

Bài D5 trong lần chạy `run.sh` đầy đủ lúc 17:00 bị bỏ qua (SKIP) vì ổ đĩa dùng chung chỉ còn 3,5 GB trống (chương trình từ chối khi dưới 3 GB + nhu cầu); D4 và D5 được chạy lại riêng bằng mã cuối cùng, kết quả ở bảng. Kiểm tra đơn vị đi kèm: `go test ./internal/reconcile ./internal/histstore ./internal/server ./cmd/staskctl`.


---

## 6. Phát hiện

### 6.1 Đã sửa (có test hồi quy)

| # | Lỗi | Sửa | Test |
|---|---|---|---|
| F2 | `histstore.openActive` cắt tệp mở tại frame sai đầu tiên như "đuôi rách" kể cả khi sau đó còn frame đúng: lịch sử mất im lặng, vị trí đã lưu vẫn trỏ về phía trước nên không bao giờ đọc lại | báo trong nhật ký, giữ phần bị cắt (`.damaged-<giờ>`), bỏ vị trí đã lưu, ghi khoảng trống vào `cover.json`. **Chưa** đọc lại xô đó từ log: xem F2b | `internal/histstore/damage_test.go` (hỏng giữa tệp: bị báo; đuôi rách thật: vẫn im lặng như cũ) |
| F8 | `GET /v1/cdc/{slot}/stream` của slot bị vô hiệu trả 200 rồi một dòng lỗi trong luồng | trả 410 Gone trước khi bắt đầu luồng | `internal/server/cdc_invalid_test.go` |

### 6.2 Tồn đọng (backlog), kèm tái hiện tối thiểu

| # | Mức | Mô tả | Tái hiện tối thiểu |
|---|---|---|---|
| F1 | **cao** (lớn, đụng đường đọc) | Byte hỏng trong đoạn log **đã niêm phong** mà checkpoint đã che không bị phát hiện lúc khởi động (recovery không phát lại đoạn đó) cũng không lúc đọc; truy vấn có thể trả giá trị sai cho tới khi `scrub` chạy, mà scrub nền **tắt theo mặc định** (`-scrub-interval 0`). Giảm nhẹ ngay: `-scrub-interval 6h` + cảnh báo `stask_scrub_errors_total`; trong cụm dựng lại nút. Sửa thật: kiểm CRC bản ghi khi đọc từ log (có chi phí, đo ở G3) hoặc quét CRC đoạn cũ lúc khởi động | `scripts/g2-drills/run.sh -drills D4 -modes single`: ca "log segment, middle of the oldest sealed segment" và hai ca "newest log segment": khởi động không một lời, `SELECT *` bảng `fill`/`ev` khác bản nguyên vẹn (hoặc không đổi nếu byte rơi vào bản ghi không còn dùng), `POST /v1/admin/scrub?wait=1` trả 500 |
| F2b | trung bình | Phần tiếp theo của F2: `Backfill` chỉ thêm những xô mà kho chưa có, nên xô bị hỏng (đã có tệp) không được đọc lại từ log dù log còn trên đĩa; `reconcile` giữ `history` `DIVERGED` (đúng, vì lịch sử thật sự thiếu) tới khi dựng lại bằng `-hist-store-backfill-dir` từ bản sao lưu. Hướng sửa: coi xô hỏng là chưa có (chuyển tệp sang `.damaged`) rồi backfill từ log, ghi khoảng trống cho phần log đã dọn | `run.sh -drills D4 -modes single`: ca "history store segment", dòng "FINDING F2b" |
| F4 | thấp | Sau SIGCONT, leader cũ vẫn trả 200 cho `/v1/ready?write=1` và `GET /v1/cluster` `writable:true` (khi `role:follower`, `lease:false`) trong ~35 ms cho tới khi xử lý bản tin term mới; ghi gửi vào nó bị từ chối (không có đa số) | `run.sh -drills D6`: dòng "FINDING F4" |
| F5 | trung bình | Leader hết chỗ cấp segment có thể **còn sống và giữ vai trò** (nhật ký lặp `commit: cannot allocate log segment (disk full?)`), nên cả cụm từ chối ghi dù hai nút còn lại khoẻ; chỉ dừng nút mới khôi phục. Các lần khác leader tự dừng bằng `panic: raft N: storage failure` và đa số bầu lại sau ~3 s. Hướng sửa: coi lỗi cấp segment lặp lại ở leader là lỗi nặng (hạ vai trò hoặc thoát) | `run.sh -drills D3 -modes cluster` (lần chạy đầu của tôi gặp trường hợp chặn; 3 lần sau gặp fail-stop): nút 3 trên tmpfs 96 MB + chặn đệm, buộc nút 3 làm leader, bơm dòng 3 KB |
| F7 | thấp | `/v1/health` luôn 200 khi từ chối ghi vì hết chỗ cấp segment (lỗi sạch, không đặt `write_failed`); giám sát chỉ bằng health sẽ không thấy | `run.sh -drills D3 -modes single`: dòng `GET /v1/health -> 200` |
| F6 | tài liệu | DSN nhiều host của pgx/libpq: `connect_timeout` bao cả danh sách nên host đầu bị đóng băng làm thất bại việc tìm leader; client có kết nối sẵn tới leader đã đóng băng treo đến khi hết timeout của chính nó. Không phải lỗi engine; cần ghi vào hướng dẫn DSN (`docs/21` §3.3) | `run.sh -drills D6` (lần đầu workload dùng DSN nhiều host: 0 giao dịch xác nhận trong lúc đóng băng) |
| F10 | thấp | Xoá thư mục dữ liệu của một thành viên cụm rồi khởi động lại **cùng id không `-raft-join`** để nó tự nạp lại không hoạt động: nút ở lại rỗng (`raft log 1..0`) và không nhận gì từ leader trong 180 s (leader ghi `write: broken pipe` tới nút đó). Quy trình có tài liệu (`cluster remove` -> xoá -> `-raft-join` -> `cluster add`) hoạt động (D4 cụm). Chỉ cần cảnh báo trong sổ tay; chưa điều tra vì sao leader không gửi snapshot | bỏ bước `cluster remove/add` khỏi `reseed` trong `scripts/g2-drills/drills_corrupt.go` |
| F9 | thấp | Kho lịch sử không hỗ trợ nút Raft (`-hist-store` bị từ chối), nên cụm không có `history` để đối chiếu | `staskd -raft-id 1 ... -hist-store` |

## 7. Chưa chứng minh

- Mọi bài chạy trên một VM 8 vCPU dùng chung và ổ đĩa duy nhất, ba nút "cụm" là ba tiến trình trên cùng máy: **không** chứng minh chịu lỗi mạng thật giữa các máy, đĩa riêng từng nút, hay đồng hồ lệch (chỉ SIGSTOP/SIGCONT; không có phân vùng mạng, `docs/06` §5.2 có `TestProcChaos`/`TestHASplitBrain` cho phần đó).
- D1 chạy 10 chu kỳ mỗi chế độ; D34-2 đòi "lặp lại dài ngày": chạy lại `run.sh -drills D1,D2 -cycles 200` ngoài giờ. Điểm giết là ngẫu nhiên theo thời gian, không nhắm vào từng pha (ghi checkpoint, lưu view, GC) ngoài việc cứ chu kỳ thứ tư giết lần hai trong lúc recovery.
- D4 lật **một byte** mỗi ca ở một vị trí giữa tệp; chưa có kiểm ngẫu nhiên hàng loạt (fuzz) vị trí/kiểu hỏng, chưa có hỏng nhiều byte, mất tệp, tệp cắt cụt, hay hỏng `tri/` (kho posting trigram trên đĩa).
- D3 dùng tmpfs 96 MB: chưa thử đĩa đầy giữa lúc ghi checkpoint hay `columnar.view` lớn (checkpoint 8 MB có chạy trong lúc đầy nhưng không bị bắt gặp giữa chừng), chưa thử lỗi `EIO` (xem `staskchaos -mode write-enospc/mixed` trong `accept/kill9.sh`).
- D5: 5 triệu dòng, một bảng, dữ liệu nén gọn (192 MB); chưa thử bản sao dữ liệu prod 30 đến 50 triệu dòng, sao lưu cụm (`-cluster`) và PITR cụm (`TestHAClusterPITR` có, drill này không lặp lại), kho lưu trữ mã hoá (`-archive-key-file`) và thời gian khôi phục thật: các số thời gian chỉ để tham khảo.
- `reconcile`: không so nội dung `search`/`timeseries`; chỉ mục trigram trên đĩa được so qua giá trị cột chứ chưa so posting với giá trị; bảng view cột có khoá chuỗi chỉ có tổng kiểm cả bảng; chưa so chéo giữa các nút (mỗi nút so với core của chính nó); chưa đo chi phí trên bảng lớn (đọc tuyến tính, chia khúc).
- `hist-store` chỉ nút đơn; chưa kiểm hỏng `seg-*` đã niêm phong của kho lịch sử.
