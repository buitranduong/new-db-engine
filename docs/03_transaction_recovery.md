# 03 — Thiết kế giao dịch, log và phục hồi (prototype)

Mã: `internal/wal`, `internal/core`, `internal/proj`, `internal/planner`.

## 1. Định nghĩa chính xác

| Câu hỏi (mục 4.1 tài liệu gốc) | Trả lời trong prototype |
|---|---|
| Điều gì là dữ liệu có thẩm quyền? | Chuỗi bản ghi log hợp lệ liên tiếp (CRC32C đúng, LSN liên tục) tính từ đầu. Không có gì khác. |
| Khi nào một thay đổi được xem là đã commit? | Khi `fdatasync` của segment chứa bản ghi trả về thành công. ACK chỉ gửi sau đó. |
| Phục hồi không mất dữ liệu đã commit? | Mọi bản ghi đã ACK nằm trong tiền tố đã fsync; phục hồi đọc tiền tố hợp lệ dài nhất và cắt bỏ phần sau. |
| Biểu diễn nội tại cập nhật/tái tạo thế nào? | Follower đọc log theo vị trí; bootstrap = quét snapshot MVCC của core tại LSN *s*, rồi áp dụng bản ghi LSN > *s*. Có thể xoá và dựng lại bất cứ lúc nào. |
| Tránh dùng góc nhìn cũ cho quyết định cần dữ liệu mới? | Mỗi view công bố `applied LSN`; planner so với `durable LSN` hoặc token phiên và chờ/chuyển sang core. Kết quả luôn kèm `plan` (engine, lsn, độ trễ, lý do). |

## 2. Định dạng log

Segment `NNNNNNNN.wal`, cấp phát trước bằng `fallocate` (mặc định 256MB) → đầy đĩa lộ ra **khi tạo segment**, không phải giữa bản ghi.

```
frame   = u32 len | u32 crc32c(payload) | payload          (len = 0 → hết phần đã ghi)
payload = u8 kind | uvarint lsn | varint commit_ts | body
tx body = uvarint n_ops | op*
op      = u8 put/del | uvarint table | key | prev(seg,off,idx) | uvarint vlen | value
```

`prev` trỏ tới phiên bản trước của cùng khoá → chuỗi phiên bản nằm ngay trong log (lịch sử, `as_of`).

## 3. Commit (đường ống hai tầng, group commit)

Commit chạy qua hai goroutine chồng lên nhau (`internal/core/engine.go`):

- **Committer** chuẩn bị từng yêu cầu ngay khi nó đến, đưa vào *lô mở*:
  1. Kiểm tra xung đột theo thứ tự: lô mở → lô đang fsync (đã ghi nhưng chưa cài vào chỉ mục) → head đã cài.
     - SI: mọi khoá ghi phải có `head.LSN ≤ snapshot` (first-committer-wins).
     - Serializable: thêm điều kiện đó cho mọi khoá đã đọc, kể cả khoá không tồn tại, để chống phantom cho đọc điểm. Scan bị từ chối trong chế độ này.
  2. Gán LSN liên tiếp, timestamp và mã bảng (DDL), rồi mã hoá vào buffer. Nếu segment không đủ chỗ thì `Roll`: fsync segment cũ và chuyển sang segment mới. Roll lỗi vì ENOSPC thì các giao dịch còn lại nhận lỗi sạch và engine vẫn sống.
- **Syncer**, khi rảnh, nhận lô mở:
  1. Committer `pwrite` phần còn lại của lô và ghi nhận file + vị trí cuối.
  2. Syncer `fdatasync` đúng file đó. Lỗi ở bước này, hoặc một lần sync khác đã lỗi trong lúc lô đang chờ, dẫn tới **fail-stop ghi** (`ErrUnknownOutcome`): không retry fsync, đọc vẫn phục vụ, cần restart.
  3. Cài phiên bản vào B-tree theo thứ tự LSN, *sau đó* công bố `durable LSN` và vị trí cuối của chính lô đó. Đây là bất biến I2: người đọc với snapshot = durable LSN không bao giờ thấy phiên bản chưa bền.
  4. ACK.

**Giao dịch lớn được chia frame.** Mỗi frame chứa tối đa 4096 thao tác và khoảng 4MB. Các frame không cuối có kiểu `KindTxPart`; tất cả dùng chung một LSN và nằm liên tiếp trong log, kể cả khi vắt qua segment. Frame cuối có kiểu `KindTx`.
- Recovery giữ các `KindTxPart` lại và chỉ áp dụng khi gặp frame cuối. Nếu log kết thúc giữa chuỗi, giao dịch đó chưa từng commit và log bị cắt từ frame đầu tiên của chuỗi.
- Nếu không thể nối tiếp chuỗi (ví dụ ổ đầy khi chuyển segment giữa chuỗi) thì engine dừng ghi (fail-stop). Lý do: một chuỗi bị bỏ dở sẽ che mọi bản ghi phía sau khỏi recovery.
- Trước khi sửa, một giao dịch trên 64MB (ví dụ `UPDATE` toàn bảng 1M dòng) được ghi thành một bản ghi mà bộ đọc coi là rách. Hậu quả là mất giao dịch đó và mọi giao dịch sau nó khi khởi động lại; `Loc.Idx` 16 bit cũng bị tràn. Hai test hồi quy: `TestLargeTransactionRecovers`, `TestIncompleteFrameChainDiscarded`.

**Ba tầng: committer → syncer → installer; trả lời ngay sau fsync.** Syncer làm fdatasync, đăng ký bảng mới (DDL), đưa lô vào **lớp phủ đọc** (overlay), công bố `durable LSN` rồi trả lời client. Installer cài lô vào chỉ mục theo thứ tự LSN, công bố `installed LSN`, rồi gỡ lô khỏi lớp phủ. Bất biến I2 (đã xác nhận thì đọc thấy) được giữ như sau:
- Đọc điểm (`head`) tra lớp phủ trước, mới đến chỉ mục. Lớp phủ chỉ gồm 1–2 lô, và map `pending` của chúng không đổi sau khi giao đi.
- Quét (`ScanRange`, `ScanSnapshot`) chờ đến khi `installed ≥ snapshot`; thường chỉ vài ms.
- Checkpoint bắt đầu từ điểm đã cài (`InstalledPoint`), không phải điểm đã bền. Nếu không, bản sao chỉ mục trong checkpoint có thể thiếu những lô đã xác nhận nhưng chưa cài.
- GC `drain` chờ đến khi các lô chuyển chỗ phiên bản được cài xong, vì remap chỉ có hiệu lực từ lúc cài.
- Kênh tới installer không có buffer: khi installer chậm, syncer tự dừng lại chờ, nên lớp phủ luôn nhỏ.

**Chỉ mục bất biến, đọc không khoá.** Mỗi bảng công bố con trỏ tới một B-tree không bao giờ bị sửa nữa (`atomic.Pointer`). Installer `Clone()` cây (O(1), copy-on-write), cài cả lô vào bản sao, rồi công bố. Người đọc không cần khoá, còn quét và checkpoint dùng thẳng cây đang công bố, không phải clone. Trước thay đổi này, 32 phiên lấy `RLock` liên tục, làm installer (cần `Lock`) chỉ đạt khoảng 350k dòng/s; INSERT 10M dòng (lô 1000) từ 27.5s giảm xuống 23.0s.

**INSERT autocommit không tra trước.** Một câu INSERT không có `ON CONFLICT`, chạy như một giao dịch riêng, không gọi `tx.Get` cho từng dòng nữa. Trùng khoá được commit phát hiện (`KeyExistsError` → 23505, cùng thông báo như PostgreSQL); trùng khoá ngay trong câu lệnh được kiểm tra trên chính các dòng của câu lệnh. Trong giao dịch tường minh, lỗi vẫn báo ngay tại câu lệnh.

**Tra head ở phía người gửi.** Goroutine của phiên đọc `installSeq` rồi tự tra head cho từng khoá, song song với các phiên khác. Committer chỉ còn kiểm tra những lô đã cài sau thời điểm đó (giữ 64 lô gần nhất trong bộ nhớ) thay vì tra B-tree tuần tự. Syncer giữ khoá bảng cho cả dãy thao tác cài liên tiếp (tối đa 256) thay vì khoá từng dòng.

Trong lúc lô N đang fsync, lô N+1 được chuẩn bị, nên mỗi chu kỳ chỉ tốn đúng một lần `fdatasync`. Phần CPU (kiểm tra, mã hoá, ghi vào page cache) nằm ngoài đường găng.

**Gom thích ứng (`-commit-wait`, mặc định 1ms).** Đĩa chỉ flush tuần tự (đo trên VM: ~1.350 flush/s, flush song song không nhanh hơn). Vì vậy N client vòng kín tự chia thành hai nhóm luân phiên, và mỗi commit mất khoảng 2 lần fsync.

Khi syncer rảnh mà lô mở còn ít hơn số dự kiến (đã xếp hàng + vừa được trả lời), committer đợi thêm để những client vừa nhận phản hồi kịp quay lại cùng lô:
- Thời gian đợi không vượt quá một lần fsync: gom chung thì mỗi client mất ≤ đợi + fsync, trượt lô thì mất 2 lần fsync.
- Mức đợi tự học: tăng khi đợi gom được, giảm khi client quay lại chậm (ví dụ CPU bão hoà), và cứ 64 lô thì thử lại mức tối đa.
- Với 1 client thì không bao giờ đợi.

**Ghi số 0 trước (`-prezero`, mặc định bật).** Segment được ghi số 0 trước khi dùng, kể cả phần đuôi của segment đầu tiên lúc khởi động. Nhờ vậy `fdatasync` không phải chuyển trạng thái extent "unwritten" trong journal ext4: p99 của một lần flush giảm từ 1.64 xuống 0.8ms. Cái giá là log được ghi hai lần (giới hạn `-zero-rate-mb`).

Kết quả: commit 1 dòng qua giao thức PostgreSQL đạt p50 1.41ms, nhanh hơn PostgreSQL 16 trên cùng máy (xem `docs/05` §6).

## 4. MVCC đọc

- Snapshot = `durable LSN` tại thời điểm bắt đầu.
- Mỗi khoá giữ trong RAM: head + tối đa 1 mức phiên bản trước; các phiên bản cũ hơn được đọc từ log qua con trỏ `prev` (đọc cũ chậm hơn nhưng bộ nhớ bị chặn).
- Scan theo khoảng: B-tree theo thứ tự; snapshot toàn bảng: `Clone()` copy-on-write O(1) của B-tree, khoá bảng chỉ giữ trong lúc clone.

## 5. Checkpoint mờ và phục hồi

- Checkpoint ghi `(StartLSN, StartPos)` đọc nguyên tử, sau đó clone chỉ mục từng bảng và ghi ra file (chunk có CRC, giải mã song song khi tải).
- Phục hồi: tải checkpoint → replay log từ `StartPos`, bỏ qua bản ghi có LSN ≤ head đã tải (idempotent) → đúng vì head chỉ tiến.
- Kích hoạt checkpoint khi log đã ghi thêm ≥ max(`checkpoint-min-mb`, kích thước checkpoint trước) → chi phí ghi checkpoint ≤ ~1× log, và đuôi cần replay bị chặn.
- **Đuôi rách:** dừng ở frame đầu tiên sai CRC/sai LSN; chỉ chấp nhận ở segment cuối. Phần sau vị trí đó bị **punch-hole** (`FALLOC_FL_PUNCH_HOLE`) rồi fallocate lại → byte của lần ghi chưa ACK không bao giờ có thể "sống lại" khi bản ghi mới ghi đè một phần.
- Hỏng ở giữa log (không phải đuôi) → từ chối khởi động (không tự cắt dữ liệu đã commit).

## 6. Biểu diễn dẫn xuất

| View | Cấu trúc | Cập nhật | Truy vấn |
|---|---|---|---|
| Columnar | vector theo cột; string dùng dictionary; bitmap sống; map khoá→vị trí | update = đánh dấu chết + append; compaction khi > 50% chết | chunk 64K dòng song song; lọc theo cột → selection vector; hash aggregate cục bộ rồi gộp |
| Search | inverted index (BM25), postings (doc, tf), bitmap sống, trigram của từ điển | tokenize ngoài lock; áp dụng theo lô | OR/AND, fuzzy ≤1–2 edit qua trigram + Levenshtein; bỏ dấu tiếng Việt |
| Time-series | khối 8192 điểm: thời gian delta-of-delta zigzag varint, float XOR (Gorilla, căn byte), series dict | append-only (core cấm update/delete bảng `append_only`) | cắt tỉa theo min/max thời gian + tập series; giải mã song song |

Áp dụng theo lô **mỗi bảng dưới một khoá** → một giao dịch nhiều thao tác hiện ra nguyên tử trong mỗi view. Nguyên tử *giữa các bảng* trong view **không** được bảo đảm (view theo bảng); truy vấn cần điều đó dùng core.

## 7. Planner và hợp đồng nhất quán

| Mode | Ý nghĩa | Hành vi |
|---|---|---|
| `strong` | phản ánh mọi commit đã ACK trước khi truy vấn bắt đầu | chờ view đạt durable LSN (mặc định ≤2s) → nếu không, chạy trên core snapshot |
| `bounded` | trễ ≤ `max_lag_ms` | dùng view nếu đủ mới; chờ tối đa bằng bound; nếu không → core |
| `session` | phản ánh ít nhất `min_lsn` (token từ lần ghi) | chờ ≤500ms → core |
| `eventual` | dùng view như hiện có | luôn view (nếu sẵn sàng) |

Mặc định theo ma trận Addendum-A: aggregate = bounded 1s, search = eventual, time-series = session nếu có token, ngược lại bounded 500ms; điểm/khoảng = strong trên core.

## 8. Thu gom log (GC)

Giá trị chỉ nằm trong log, nên giải phóng dung lượng nghĩa là **dọn segment**: chép phần còn dùng sang đuôi log rồi xoá cả segment. Mã: `internal/core/gc.go`.

**Horizon H** = min(snapshot cũ nhất đang được ghim, durable LSN tại thời điểm `now − retention`).
- Snapshot được ghim gồm giao dịch đang mở và follower đang bootstrap.
- Một phiên bản bị thay thế tại LSN ≤ H thì không reader nào ở snapshot ≥ H nhìn thấy.
- `-history-retention` (mặc định 1h, tối thiểu 2s) là cửa sổ lịch sử cho `as_of`.

Với mỗi segment, engine theo dõi `live`: số byte của các phiên bản đang là head. Đại lượng này cập nhật ngay trong committer.

Một vòng dọn gồm bốn bước:
1. **Chọn nạn nhân.** Segment đã niêm phong và có `live ≤ 50%` (`-gc-max-live`). Ưu tiên segment ít dữ liệu sống nhất; tổng byte cần chép ≤ 1GB mỗi vòng.
2. **Chép những gì còn cần.** Đọc tuần tự segment:
   - Head nào còn nằm ở đó được chép sang đuôi log bằng bản ghi `KindMove`. Bản ghi này mang LSN và commit time gốc nên *không đổi dữ liệu*, chỉ đổi vị trí. Chỉ mục trỏ sang bản chép.
   - Phiên bản không còn là head nhưng bị thay thế **sau** H vẫn có thể có reader cần. Nó cũng được chép, kèm một **bảng remap** trong RAM (vị trí cũ → vị trí mới, cùng LSN thay thế). Khi đi theo chuỗi phiên bản mà gặp segment đã xoá, engine tra remap.
   - Tombstone có LSN ≤ H bị gỡ khỏi chỉ mục thay vì chép.
   - Mọi bản chép đi qua committer, nên tuần tự với các commit. Tốc độ chép bị giới hạn (`-gc-rate-mb`, mặc định 64MB/s).
3. **Checkpoint.** Bản clone chỉ mục phải được tạo *sau* bước 2, để không checkpoint nào còn trỏ vào nạn nhân.
4. **Xoá.** Chỉ xoá khi đủ bốn điều kiện:
   - đã có checkpoint của bước 3 trên đĩa và vị trí bắt đầu của nó nằm sau nạn nhân;
   - mọi follower đã đọc qua nạn nhân;
   - `live = 0`;
   - mọi thao tác đọc có thể đang giữ con trỏ vào nạn nhân đã xong.

   Điều kiện cuối dùng hai epoch đọc với bộ đếm phân mảnh, kiểu RCU. Trước khi chờ các lượt đọc cũ kết thúc, segment bị đánh dấu "sắp xoá". Lượt đọc bắt đầu sau thời điểm đó coi segment như đã mất và đi qua remap, kể cả khi đang theo con trỏ `prev` trong bộ nhớ. Trước đây có một khe hở ngay giữa lúc chờ và lúc xoá file: lượt đọc mới vẫn có thể lấy được phiên bản trong segment rồi gặp `segment was reclaimed`. Đó là nguyên nhân khiến `TestGCConcurrent` thỉnh thoảng lỗi. Trước khi xoá, engine nâng **floor** lên H. Đọc ở snapshot < floor trả `ErrSnapshotTooOld` (HTTP 410, SQLSTATE 72000) thay vì âm thầm thiếu phiên bản. `History` dừng ở phần lịch sử đã thu hồi. Mục remap được bỏ khi LSN thay thế của nó ≤ floor.

Phục hồi:
- `KindMove` được replay theo kiểu "chỉ đổi vị trí nếu head vẫn có LSN gốc", nên idempotent.
- Các segment đã xoá luôn nằm trước điểm bắt đầu của checkpoint mới nhất, nên replay không bao giờ cần đến chúng.
- `live` được tính lại từ chỉ mục khi khởi động.
- Bảng remap không lưu trên đĩa. Sau khởi động lại, đọc `as_of` vào phần lịch sử từng được remap sẽ nhận 410 thay vì dữ liệu. Kết quả không bao giờ sai, nhưng cửa sổ lịch sử có thể ngắn hơn retention.

Thiết kế đầu tiên chỉ dọn segment mà mọi phiên bản đều bị thay thế trước H (`supMax ≤ H`). Khi cập nhật trải đều trên toàn bộ khoá, điều kiện này gần như không bao giờ đúng: benchmark WL-1 thu hồi được 0 segment. Thiết kế remap bỏ điều kiện đó.

Kết quả đo (WL-1, 200k khoá, 128 luồng, 180s, segment 64MB, retention 0):

| | Dung lượng đĩa | Số file log | Vòng thu hồi |
|---|---|---|---|
| GC tắt | 2531MB | 39 | 0 |
| GC bật | 1383MB | 21 | 21 |

Kiểm chứng:
- `TestGCReclaimsAndRecovers`, `TestGCRespectsPinnedSnapshot`.
- `TestGCConcurrent`: ghi, giao dịch đọc, quét và follower chạy cùng lúc với các vòng GC liên tục.
- `TestPowerLossDuringGC`: 40 lần cắt điện mô phỏng trong lúc GC đang chép và xoá.
- `staskchaos -hot 20`: 30 chu kỳ kill, ENOSPC, EIO khi GC chạy mỗi giây.

## 9. Giới hạn đã biết của thiết kế hiện tại

- GC chép lại dữ liệu sống (ghi thêm ≈ live/(1−live) cho mỗi byte thu hồi) và cần một checkpoint đầy đủ mỗi vòng. Chưa phân tầng nóng/lạnh và chưa có checkpoint tăng dần. Bảng remap nằm trong RAM và mất khi khởi động lại.
- Chỉ mục chính + view nằm trong RAM.
- Một node, không replication → chưa kiểm chứng phân vùng mạng.
- SQL là một tập con (xem `docs/05_sql_pgwire.md`): một bảng mỗi truy vấn, không JOIN/subquery, không index phụ, không `pg_catalog`.
- Giao dịch tương tác qua HTTP giữ trạng thái ở server (hết hạn sau 30s không dùng).
