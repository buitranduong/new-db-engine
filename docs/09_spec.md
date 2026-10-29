# 09 — Đặc tả độc lập ngôn ngữ (chuẩn bị port Rust)

Bản Rust phải là **bản thay thế tương thích**:
- đọc được thư mục dữ liệu do bản Go ghi, và ngược lại;
- làm replica cho leader Go, và ngược lại;
- trả cùng kết quả SQL.

Tài liệu này mô tả mọi thứ được lưu bền hoặc gửi qua mạng, đủ để viết lại mà không cần đọc code Go. Code Go là bản tham chiếu: nơi nào tài liệu và code khác nhau thì code đúng, và tài liệu phải sửa. Phần ngữ nghĩa giao dịch nằm ở docs/03; ở đây chỉ nhắc những điểm ảnh hưởng tới định dạng.

**Oracle định dạng:** `cmd/stasklogdump -dir <thư mục> [-rows]` in toàn bộ trạng thái bền của một thư mục đã dừng ở dạng chuẩn hoá, mỗi dòng một JSON:
1. checkpoint mới nhất: header, rồi mọi entry theo thứ tự trong file;
2. mọi bản ghi của mọi segment theo thứ tự log;
3. điểm kết thúc của từng segment.

Bản Rust có lệnh tương đương; hai lệnh phải in **cùng từng dòng** trên cùng một thư mục (mục 10).

## 1. Mã hoá cơ bản

| Ký hiệu | Nghĩa |
|---|---|
| `u8/u16/u32/u64/i64` | số nguyên kích thước cố định, **little endian** |
| `uvarint` | LEB128 không dấu, như `encoding/binary.AppendUvarint` của Go ([định dạng varint của Protocol Buffers](https://protobuf.dev/programming-guides/encoding/#varints)); tối đa 10 byte |
| `varint` | số có dấu mã hoá zigzag rồi uvarint (`binary.AppendVarint`: `(n << 1) ^ (n >> 63)`) |
| `crc32c` | CRC-32 đa thức Castagnoli ([RFC 3720 §B.4](https://www.rfc-editor.org/rfc/rfc3720#appendix-B.4)), không đảo bit thêm ngoài chuẩn |
| `bytes(n)` | n byte thô |

Mọi trường JSON dùng UTF-8, theo `encoding/json` của Go. Thứ tự trường không quan trọng khi đọc. Khi ghi, bản Rust nên giữ đúng thứ tự trường như struct Go, để checkpoint của hai bản giống từng byte (mục 10).

## 2. Thư mục dữ liệu

```
<dir>/wal/NNNNNNNN.wal              segment thô, N = số segment, 8 chữ số thập phân
<dir>/wal/NNNNNNNN.walz             segment đã nén (mục 6)
<dir>/wal/NNNNNNNN.walz.tmp         nén dở dang: xoá khi mở
<dir>/ckpt/ckpt-LLLLLLLLLLLLLLLLLLLL.ckpt   checkpoint, L = StartLSN, 20 chữ số
<dir>/ckpt/ckpt.tmp                 checkpoint đang ghi: bỏ qua
<dir>/columnar.view                 view cột đã nén, gắn LSN (mục 7a)
<dir>/columnar.view.tmp             view đang ghi: bỏ qua, ghi đè ở lần lưu sau
<dir>/replica.json                  chỉ có trên replica (mục 8)
<dir>/slots/<tên>.json              slot CDC (mục 9), chỉ có trên leader
<dir>/cdc.json                      publication và replica identity (mục 9), chỉ có trên leader
```

Quy tắc khi mở:
- Nếu cùng một số có cả `.wal` và `.walz`, thì máy đã sập sau khi cài bản nén nhưng trước khi xoá bản thô. Cả hai đều đầy đủ: giữ `.walz`, xoá `.wal`.
- Checkpoint được dùng là file có L lớn nhất.

## 3. Segment log

**Cấp phát trước.** Segment được tạo đủ kích thước `segMax` (mặc định 256 MiB, tối đa 2^32) bằng `fallocate`, nên phần chưa ghi toàn là byte 0. Trước khi tạo segment mới, segment trước đã được `fdatasync`, và thư mục được fsync sau khi tạo.

**Frame:**

```
frame   = u32 len | u32 crc32c(payload) | payload[len]
payload = u8 kind | uvarint lsn | varint ts | body
```

- `len = 0` (hoặc dưới 8 byte còn lại toàn 0) là **hết phần đã ghi** của segment.
- `len > 64 MiB` là frame hỏng.
- `ts` là thời điểm commit, tính bằng nano giây Unix, tăng nghiêm ngặt theo LSN (đồng hồ lùi thì lấy `ts` trước + 1).

| kind | Tên | body |
|---|---|---|
| 1 | `Tx` | `uvarint nops`, rồi `op × nops` |
| 2 | `DDL` | JSON của một bảng (mục 5), tới hết payload |
| 3 | `Move` | như `Tx`, nhưng mỗi op có tiền tố `uvarint origLSN, varint origTS` |
| 4 | `TxPart` | như `Tx`; là frame không cuối của một giao dịch lớn |

```
op   = u8 opkind (1 = put, 2 = del) | uvarint table | uvarint klen | key[klen]
     | prev | uvarint vlen | value[vlen]
prev = uvarint 0                                     (không có phiên bản trước)
     | uvarint (seg + 1) | uvarint off | uvarint idx (vị trí phiên bản trước)
```

Một payload hợp lệ phải được đọc **vừa hết**. Thừa hoặc thiếu byte nghĩa là frame hỏng.

**Vị trí (`Loc`).** `(seg u32, off u32, idx u16)`:
- `off` là offset của **đầu frame** trong segment;
- `idx` là thứ tự op trong frame.

Đây là địa chỉ duy nhất của một phiên bản. Index, checkpoint, `prev` và replica đều dùng nó. `ValOff` là offset tuyệt đối của byte đầu tiên của value trong segment: `off + 8 + vị trí trong payload`.

**Giới hạn khi ghi.** Một frame có tối đa 4096 op và tối đa 4 MiB (`MaxFrameOps`, `MaxFrameBytes`); giao dịch lớn hơn bị tách:
- các frame `TxPart` liên tiếp nhau, dùng chung một LSN;
- frame cuối cùng có kind `Tx`;
- chuỗi `TxPart` không có frame `Tx` ở cuối là giao dịch **chưa commit**.

**Bất biến khi đọc lại (recovery):**
1. LSN của các bản ghi đã commit liên tục, tăng đúng 1. Mọi frame trong một chuỗi TxPart mang cùng LSN của giao dịch.
2. Gặp frame sai CRC hoặc sai LSN thì dừng. Điều này chỉ được chấp nhận ở segment cuối (hoặc segment áp chót, nếu segment cuối rỗng vì là segment dự phòng tạo trước). Ở chỗ khác thì **từ chối khởi động**: hỏng giữa log không được tự cắt.
3. Phần sau điểm dừng bị huỷ: hole-punch phần còn lại của segment đó (`FALLOC_FL_PUNCH_HOLE`, rồi cấp phát lại) và xoá các segment sau. Nhờ vậy byte của một lần ghi chưa ACK không thể "sống lại" khi bị ghi đè một phần.
4. Nếu log kết thúc giữa một chuỗi TxPart, thì điểm cuối của log là **frame đầu** của chuỗi đó.
5. Một segment sau không có bản ghi nào (segment dự phòng) không làm dời điểm cuối.

**Commit.** Một giao dịch commit khi `fdatasync` của segment chứa frame `Tx` của nó trả về thành công. ACK chỉ gửi sau đó; lỗi fsync là lỗi chết (docs/03).

**Op `Move`.** Khi GC thu hồi một segment, nó chuyển các phiên bản còn cần sang cuối log:
- `origLSN`/`origTS` là danh tính gốc của phiên bản. Phiên bản không đổi, chỉ đổi chỗ.
- `prev` giữ nguyên chuỗi phiên bản.
- Bản ghi Move có LSN riêng, nằm trong dãy liên tục.

## 4. Dòng và khoá

**Kiểu cột** (tên trong JSON của bảng):

| Tên | Mã | Mã hoá giá trị |
|---|---|---|
| `int` | 1 | varint |
| `float` | 2 | u64 (bit IEEE-754) |
| `string` | 3 | uvarint len, bytes |
| `text` | 4 | uvarint len, bytes |
| `time` | 5 | varint, mili giây Unix |
| `bool` | 6 | varint 0/1 |

**Dòng:**
- Bitmap NULL trước: `ceil(ncol / 8)` byte, bit `i % 8` của byte `i / 8` bật nếu cột i là NULL.
- Sau đó là các cột không NULL theo thứ tự khai báo. Sau ALTER TABLE, "các cột" là bố cục tại LSN của dòng (§5).
- Value của op `del` rỗng.

**Khoá:**
- Là chuỗi byte. Khoá `int` được lưu bằng số thập phân (`strconv.FormatInt`).
- Index sắp theo **thứ tự byte** của khoá. Vì vậy khoá int không theo thứ tự số, và tầng SQL tự xử lý việc đó.
- `table` trong op là `ID` của bảng.

## 5. DDL và catalog

Bản ghi `DDL` chứa JSON của bảng, cũng là phần tử của `tables` trong header checkpoint:

```json
{"id":1,"name":"orders","columns":[{"name":"id","type":"int","not_null":true}, ...],
 "key":"id","search":["note"],"columnar":true,"append_only":true,
 "time_column":"created","series_column":"host"}
```

- Các trường có `omitempty` (`not_null`, `search`, `columnar`, `append_only`, `time_column`, `series_column`) bị bỏ khi mang giá trị rỗng.
- Kiểm tra hợp lệ ở `schema.Table.Init`: khoá là `int` hoặc `string`; cột search là `text`/`string`; `time_column` là `time` và đòi hỏi `append_only`; `series_column` là `string`.
- **DROP TABLE và TRUNCATE** (thêm 2026-10-29) dùng chính bản ghi `DDL`, với ba trường tuỳ chọn:
  - `dropped_lsn`: bản ghi của DROP TABLE là JSON của bảng bị drop (cùng `id`), có `dropped_lsn` bằng LSN của chính bản ghi.
  - `replaces`: bản ghi của TRUNCATE là một bảng **mới** (ID mới, cùng tên và cột) với `replaces` là ID bảng cũ. Khi phát lại, bảng cũ bị coi là drop tại LSN của bản ghi này. Một bản ghi duy nhất nên không thể chỉ ghi được một nửa.
  - `rel_id`: định danh ổn định cho client (OID của PG = 16384 + `rel_id`, hoặc + `id` nếu không có). TRUNCATE giữ nguyên nó.
  - Bảng đã drop vẫn nằm trong `tables` của checkpoint (có `dropped_lsn`), vì các bản ghi cũ trong log và CDC còn tham chiếu ID của nó. Tên của nó không còn chiếm chỗ: một bảng mới có thể dùng lại tên.
  - Bản ghi op của bảng đã drop không bao giờ xuất hiện sau bản ghi drop, trừ bản ghi `Move` của GC (phát lại vô hại). GC chỉ thu hồi các version đó khi horizon đã qua `dropped_lsn`, vì trước đó snapshot cũ còn có thể đọc.
- **ALTER TABLE** (thêm 2026-10-29) cũng là bản ghi `DDL`, mang JSON của bảng sau khi đổi với **cùng `id`**. Khi phát lại, bản ghi có `id` đã tồn tại thì thay schema của bảng đó. Không dòng nào bị ghi lại; dòng được giải mã theo bố cục có hiệu lực tại LSN commit của version chứa nó. Với bản ghi `Move` của GC thì dùng `orig_lsn`, tức LSN gốc. Các trường của cột:
  - `num`: số thứ tự vật lý của cột (như `attnum` của PG). Tăng dần theo `columns` và không dùng lại. Thiếu (bảng cũ) thì là 1, 2, … theo thứ tự.
  - `added_lsn`: LSN của ADD COLUMN, 0 nếu cột có từ khi tạo bảng. Dòng lưu trước LSN này không chứa cột và đọc ra `missing` (giá trị DEFAULT tính một lần lúc ALTER, như `attmissingval`), hoặc NULL.
  - `dropped_columns` (của bảng): các cột đã DROP COLUMN, mỗi cột giữ `num`, `type`, `added_lsn` và có thêm `dropped_lsn`. Dòng lưu trước `dropped_lsn` vẫn chứa giá trị của cột; bộ giải mã đọc rồi bỏ qua giá trị đó.
  - **Bố cục tại LSN L**: mọi cột (cả đang sống lẫn đã drop) có `added_lsn ≤ L` và chưa drop tại L (`dropped_lsn` = 0 hoặc > L), xếp theo `num`. Bitmap NULL và giá trị đi theo đúng bố cục này. Dòng ghi mới luôn dùng các cột đang sống.
  - Schema chỉ được công bố cho người ghi khi DDL được đặt vào log (prepare). Từ lúc đó, dòng đã mã hoá theo schema cũ sẽ bị `ErrConflict` khi commit, và autocommit tự làm lại. Checkpoint ghi schema của DDL đã bền, không ghi schema đang chờ.
- **Index phụ và khoá ngoại** (thêm 2026-10-29) là hai trường của JSON bảng, ghi bằng bản ghi `DDL` kiểu ALTER (cùng `id`, thêm hoặc bỏ phần tử). Cả hai bỏ khi rỗng, nên dữ liệu cũ đọc được nguyên vẹn:
  - `indexes`: `[{"name":"orders_cust_idx","columns":["cust","created"],"unique":true,"constraint":"orders_uq","internal":true}]`. `constraint` có khi index là của một ràng buộc UNIQUE; `internal` khi do khoá ngoại tạo cho cột con.
  - `foreign_keys`: `[{"name":"orders_cust_fkey","columns":["cust"],"ref_table":"customers","ref_columns":["id"],"on_delete":"cascade"}]`; `on_delete` là `no action` (mặc định, có thể vắng), `restrict`, `cascade` hoặc `set null`. `ref_table` là tên bảng cha.
  - **Entry của index không nằm trong log hay checkpoint**: chỉ định nghĩa. Entry có dạng `Enc(cột 1) ‖ ... ‖ khoá chính` (mã hoá ở `schema/indexkey.go`, mô tả ở docs/05 §18) và được dựng lại từ các đầu của index chính khi khởi động, khi replica nhận DDL, hoặc khi `CREATE INDEX`. Bản Rust cần cùng mã hoá thứ tự nếu muốn so khớp với bản Go qua `pg_index`, nhưng định dạng đĩa không phụ thuộc vào nó.
- Catalog **chỉ** gồm các bảng này. Tầng SQL không lưu gì thêm: kiểu PostgreSQL (`bigint`, `timestamptz`, …) được ánh xạ sang 6 kiểu ở trên khi `CREATE TABLE`, và `WITH (...)` thành các cờ ở trên.

## 6. Segment nén (`.walz`)

Segment đã đóng và cũ hơn `compress-keep-raw` có thể được viết lại thành các khối nén độc lập. Các khối được địa chỉ hoá theo **offset thô**, nên `Loc`/`ValOff` không đổi.

```
block × n                       mỗi khối là zBlock = 16 KiB thô, nén độc lập
table: u64 × (n + 1)            offset trong file của từng khối, rồi offset kết thúc
footer (33 byte): u64 rawLen | u32 blockSize | u8 codec | u32 n | u64 tableOff
                | u32 crc32c(table) | "STZ1"
```

- `codec`:
  - 0 = không nén;
  - 1 = [S2](https://github.com/klauspost/compress/tree/master/s2), dạng block;
  - 2 = [zstd](https://www.rfc-editor.org/rfc/rfc8878), mỗi khối là một frame.
- File hợp lệ khi:
  - `tableOff + 8(n + 1) = kích thước file − 33`;
  - `n = ceil(rawLen / blockSize)`;
  - CRC của bảng offset khớp.

**Rủi ro cho port Rust:** S2 là phần mở rộng của Snappy (có thêm repeat offset). Bộ giải mã Snappy **không** đọc được S2 ([tài liệu gói s2](https://pkg.go.dev/github.com/klauspost/compress/s2)). Rust có crate [`minlz`](https://docs.rs/crate/minlz/1.1.0/source/README.md), tự nhận là giải mã mọi luồng S2 của Go và từ bản 1.1 thì mã hoá giống từng byte. Đây là tuyên bố của tác giả, **chưa được kiểm chứng độc lập**. Bộ nghiệm thu (mục 10) phải đọc checkpoint và `.walz` do Go ghi. Nếu `minlz` không qua, có hai đường lui:
- ghi checkpoint v4 bằng `s2.EncodeSnappy` (Snappy chuẩn, mọi bản Snappy đều đọc được);
- dùng zstd cho `.walz`.

## 7. Checkpoint (phiên bản 3)

```
u32 hdrLen | header JSON[hdrLen]
chunk*     : u32 len | u32 crc32c(payload) | payload
u32 0      (kết thúc)
header     = {"version":3,"start_lsn":L,"start_seg":S,"start_off":O,"last_ts":T,
              "next_table_id":N,"tables":[<bảng như mục 5>...]}
payload    = uvarint tableID | uvarint n | S2-block(entries)
entry      = uvarint klen | key | uvarint zigzag(lsn − lsnTrước) | uvarint seg
           | uvarint off | uvarint idx | uvarint (valOff − off) | uvarint valLen
           | u8 flags (bit 0 = đã xoá)
```

- `lsnTrước` bắt đầu bằng 0 ở **mỗi chunk**.
- Mỗi chunk có tối đa 65.536 entry của một bảng. Các chunk của một bảng liên tiếp nhau, và entry sắp tăng nghiêm ngặt theo thứ tự byte của khoá. Thứ tự sai thì checkpoint bị từ chối.
- Checkpoint chỉ chứa index chính; index phụ được mô tả trong `tables[].indexes` của header và dựng lại sau khi nạp xong (docs/05 §18).
- Mỗi entry là **đầu** (phiên bản mới nhất) của một khoá; tombstone cũng được ghi. TS và vị trí phiên bản trước không được lưu: chúng nằm trong bản ghi log tại `Loc`.
- Checkpoint là "mờ" (fuzzy):
  - `(start_lsn, start_seg:start_off)` là điểm bền vững đọc được **trước khi** clone index;
  - recovery tải checkpoint rồi phát lại log từ `start`, bỏ qua bản ghi không mới hơn đầu đã tải của khoá;
  - vì đầu của một khoá chỉ tiến, kết quả bằng trạng thái ở cuối log.
- Ghi checkpoint: ghi ra `ckpt.tmp`, fsync, đổi tên thành tên cuối, fsync thư mục, rồi xoá checkpoint cũ.
- Đọc phiên bản cũ: v1 không delta và không nén; v2 giống v3 nhưng mỗi entry có thêm `varint ts` sau lsn, và `flags` bit 1 báo có thêm 3 uvarint `prev`. Bản Rust chỉ cần đọc v3, trừ khi phải mở dữ liệu cũ.

## 7a. Tệp view cột (`columnar.view`)

View cột là dữ liệu dẫn xuất từ log; tệp này chỉ là bản lưu để khỏi dựng lại. Bản Rust không bắt buộc đọc nó: bỏ qua tệp thì dựng lại từ dữ liệu, kết quả như nhau.

```
"STCV" u32 version(=1)
section*   : u8 tag | u64 len | payload | u32 crc32c(tag, len, payload)
tag 1 header : u64 lsn | u32 seg | i64 off | u32 ntables
tag 2 table  : u32 id | u64 rows | u32 ncols | ncols x (i32 num | u8 type | u8 flags)
               flags: bit 0 = cột được lưu, bit 1 = cột TEXT
tag 3 column : u32 chỉ số cột | u8 hasNulls [u32 len | zstd(bitmap u64 LE)]
               [chuỗi/TEXT: u32 nvals | u32 len | zstd(uvarint len | bytes)*]
               u32 nblocks | (u32 len | block)*
tag 4 keys   : (bảng có khoá không phải số nguyên) u32 nblocks | (u32 len | zstd(uvarint len | bytes)*)*
tag 5 end    : u32 ntables
```

- Thứ tự: header, rồi với mỗi bảng (id tăng dần) một section 2, các section 3 theo thứ tự cột, section 4 nếu có; cuối cùng section 5. Thiếu section 5 hoặc sai crc nghĩa là tệp hỏng.
- `(lsn, seg:off)` là trạng thái mà view phản ánh và vị trí log ngay sau LSN đó.
- Chỉ ghi các dòng còn sống, đánh số lại từ 0. Mỗi block là giá trị của tối đa 65.536 dòng, nén độc lập. Block số nguyên/thời gian/bool/mã từ điển: `zstd(u8 kind | u8 bits | u32 n | i64 base | i64 aux | residual)`.
  - kind 0 = frame-of-reference (`x − min`, base = min); 1 = delta (zigzag(x[i] − x[i−1]), base = x[0]); 2 = delta-of-delta (zigzag(d[i] − d[i−1]), base = x[0], aux = x[1] − x[0]).
  - residual đóng gói từng bit ở độ rộng `bits` của giá trị lớn nhất (0 = hằng số, không có byte nào); trên 56 bit thì mỗi giá trị 8 byte. Chọn kind có `bits` nhỏ nhất, hoà thì kind nhỏ hơn.
- Block số thực bắt đầu bằng một byte kind: 0 = các giá trị 8 byte thô rồi zstd; 1 = tách theo byte-plane rồi zstd; 2 = thập phân (`u32 scale`, rồi block số nguyên của `round(x·scale)`), chỉ dùng khi mọi giá trị đúng bằng `float64(k)/scale` theo từng bit (loại −0, NaN, Inf). Với kind 0/1 giữ cái nhỏ hơn.
- Cột TEXT đã bị bỏ khỏi view vì quá 65.536 giá trị khác nhau (flags bit 0 = 0, bit 1 = 1) thì không có section 3.
- Ghi: `columnar.view.tmp`, fsync, đổi tên, fsync thư mục. Sập giữa chừng thì còn nguyên tệp cũ hoặc tệp mới.
- Điều kiện dùng khi mở (mọi điều kiện sai thì dựng lại từ dữ liệu, không lỗi): tệp đọc được trọn vẹn với mọi crc đúng; `lsn` không vượt LSN bền; segment `seg` còn; bản ghi đầu tiên tại `seg:off` có LSN ≤ `lsn+1`; tập bảng cột và, với mỗi bảng, số hiệu cột, kiểu cột đúng bằng schema hiện tại (ALTER/DROP sau lúc lưu làm tệp bị bỏ); không đang phục hồi từ archive hay đến một LSN/thời điểm đích. Sau khi nạp, chỉ phát lại log từ `seg:off`, bỏ bản ghi có LSN ≤ `lsn`.

## 8. Giao thức nhân bản (TCP)

```
frame = u8 type | u32 len | payload[len]      (len ≤ 256 MiB)
```

Replica mở kết nối bằng 10 byte `STASKREPL1`, rồi gửi `H`.

| type | Chiều | payload |
|---|---|---|
| `H` hello | replica → leader | `u8 fresh | u32 seg | i64 off | id` (id: phần còn lại, 16 ký tự hex) |
| `h` header | L → R | `i64 segMax` |
| `S` segment | L → R | `u32 seg | bytes` (chỉ trong base backup) |
| `C` checkpoint | L → R | `u16 nameLen | name | bytes` (chỉ trong base backup) |
| `B` base done | L → R | `u32 seg | i64 off` (điểm kết thúc log của replica) |
| `D` data | L → R | `u32 seg | i64 off | u32 oldest | u64 durable | bytes` |
| `K` heartbeat | L → R | `u32 oldest | u64 durable` (sau 1 s không có dữ liệu) |
| `A` ack | R → L | `u32 seg | i64 off` (đã bền trên replica) |
| `E` error | L → R | thông điệp |

**Base backup** (replica rỗng, `fresh = 1`):
1. Leader chụp một checkpoint mới.
2. Leader gửi `S` cho mọi segment tới điểm bền vững `dpos`. Mỗi `S` chứa các byte bản ghi của segment đó tới `dpos`.
3. Leader gửi `C` (checkpoint), rồi `B dpos`, rồi đóng kết nối.
4. Replica ghi vào `<dir>.base/`: mỗi segment được truncate lên đúng `segMax`, mọi file được fsync. Sau đó replica ghi `replica.json` `{"id","leader","segment_size"}`.
5. Replica đổi tên `wal`, `ckpt`, rồi **cuối cùng** `replica.json` vào thư mục thật. Chưa có `replica.json` thì thư mục chưa phải replica.
6. Replica mở engine (recovery như thường), rồi kết nối lại với `fresh = 0` và vị trí cuối log của nó.

**Stream:**
- Mỗi `D` chứa byte log nguyên văn, kể từ `seg:off`. Replica ghi chúng vào **đúng segment và offset đó**, nên log của replica luôn là tiền tố từng byte của log leader.
- Replica fdatasync, giải mã, cài vào index, rồi gửi `A`.
- Leader chỉ gửi những byte đã bền trên leader.
- `oldest` là segment cũ nhất leader còn giữ. `oldest` được đọc **trước** `durable`, nên mọi thứ đã bền vào lúc segment kia bị thu hồi đều ≤ `durable`. Replica chỉ xoá segment < `oldest` sau khi đã áp tới `durable`, và sau khi đã ghi một checkpoint không còn tham chiếu segment đó.

**Slot:**
- Theo `id`, giống [replication slot của PostgreSQL](https://www.postgresql.org/docs/16/warm-standby.html#STREAMING-REPLICATION-SLOTS).
- Leader giữ phần log replica còn cần tới vị trí đã ACK, trong tối đa `KeepFor` (10 phút) khi replica mất kết nối.
- Replica quay lại muộn hơn thì nhận `E` ("cần base backup mới").

## 9. CDC và LISTEN/NOTIFY

Chi tiết thiết kế và lý do ở docs/10. Phần này chỉ ghi những gì bản khác phải giữ nguyên.

**File slot** `<dir>/slots/<tên>.json`, tên khớp `^[a-z0-9_]{1,63}$`:

```json
{"name":"s","plugin":"wal2json","seg":0,"off":194,"lsn":3,"created":1793247127860027197,"invalid":"..."}
```

- `seg`/`off`: vị trí log ngay sau giao dịch cuối cùng đã tiêu thụ (hoặc điểm tạo slot). Giải mã bắt đầu từ đây.
- `lsn`: commit LSN của giao dịch đó. Engine giữ snapshot GC tại LSN này.
- `created`: Unix ns. `invalid`: có mặt khi slot đã mất log (vượt `SlotMaxLagBytes`); slot như vậy chỉ còn xoá được.
- Ghi: file tạm, fsync, rename, fsync thư mục `slots`. Slot không bao giờ lùi.

**Vị trí theo kiểu PG**: `fmt("%X/%X", seg, off)`. `xid` = commit LSN. Với mỗi giao dịch, `lsn` của BEGIN là đầu bản ghi đầu tiên của giao dịch (`TxPart` đầu tiên, hoặc `Tx` nếu chỉ có một bản ghi); `lsn` của mỗi thay đổi là đầu bản ghi chứa op đó; trong wal2json, `lsn` của dòng C là đầu bản ghi `Tx` cuối, còn `nextlsn` (và `lsn` của hàng COMMIT trong kết quả SQL / dòng HTTP) là vị trí ngay sau bản ghi `Tx` đó. Xác nhận tới X tiêu thụ mọi giao dịch có vị trí sau-bản-ghi ≤ X.

**Phân loại thay đổi**: PUT mà `prev` trỏ tới một op PUT là `U`, còn lại (`prev` rỗng hoặc trỏ tới DEL) là `I`. DEL mà `prev` trỏ tới PUT là `D`; DEL khác bị bỏ qua. Giao dịch không còn thay đổi nào sau khi lọc vẫn có BEGIN/COMMIT, trừ khi bật `skip-empty-xacts`. Bản ghi DDL và Move không sinh thay đổi, nhưng vẫn làm slot tiến.

**Định dạng thông điệp**: chuỗi do `internal/sql/cdc_test.go` cố định là chuẩn. Giá trị: số nguyên, số thực (dạng ngắn nhất, NaN/Infinity thành chuỗi) và boolean là literal JSON; timestamp là `YYYY-MM-DD HH:MM:SS[.fff]+00`; chuỗi được escape như `escape_json` của PG (chỉ `"`, `\` và ký tự điều khiển). Timestamp của commit có độ chính xác micro giây.

**HTTP**: mỗi dòng NDJSON là `{"lsn":..,"xid":..,"data":<thông điệp>[,"commit":true]}`. `data` là object JSON với wal2json (mặc định format-version 2, kèm `nextlsn` ở dòng C) và chuỗi với test_decoding. Lỗi sau khi stream đã bắt đầu là một dòng `{"error":..}`.

**`cdc.json`** (ghi nguyên tử như file slot):

```json
{"publications":[{"name":"pub","tables":["a","b"],"insert":true,"update":true,"delete":true},
                 {"name":"all","all_tables":true,"insert":true,"update":false,"delete":true}],
 "identity_full":["b"]}
```

`tables` được sắp xếp, không trùng. `identity_full` là tên các bảng có `REPLICA IDENTITY FULL`. Thiếu file thì coi như rỗng.

**Walsender** (kết nối có `replication=database`):
- Kết nối này báo `server_version` = `15.0`; kết nối thường báo `16.0`. Lý do ở docs/10 §6.
- Lệnh replication nhận diện theo từ đầu tiên, không phân biệt hoa thường; tên trong `"..."` giữ nguyên, tên không có ngoặc kép chuyển thành chữ thường.
- `START_REPLICATION`: bỏ qua mọi giao dịch có vị trí sau-bản-ghi-commit ≤ lsn yêu cầu, rồi gửi `W` (format 0, 0 cột) khi bắt đầu stream.
- Trường `walStart` và `walEnd` của mỗi XLogData là LSN 64 bit của thông điệp (cuối giao dịch với COMMIT, đầu bản ghi với các thông điệp khác); `sendTime` là µs kể từ 2000-01-01.
- Keepalive `k` gửi cuối giao dịch cuối cùng đã giải mã xong (gửi hoặc bỏ qua), không phải vị trí bền.
- Standby status `r`: trường flush > 0 thì xác nhận slot tới đó (như `ConfirmSlot`); byte cuối = 1 thì trả ngay một keepalive.
- Client gửi CopyDone `c` thì server trả `c`, `C "COPY 0"`, `C "START_REPLICATION"`, rồi `Z`.
- Lỗi trước khi gửi `W` là ErrorResponse thường và kết nối vẫn dùng được; lỗi sau đó thì đóng kết nối. Slot đang có consumer khác: SQLSTATE `55006`.

**pgoutput**:
- LSN 64 bit = `seg << 32 | off`.
- OID của bảng = 16384 + id bảng. OID kiểu: int8 20, float8 701, text 25 (cả hai kiểu chuỗi), bool 16, timestamptz 1184; typmod -1.
- `B`: final_lsn = đầu bản ghi `Tx` cuối, thời điểm commit, xid = 32 bit thấp của commit LSN. Chỉ gửi trước thay đổi đầu tiên thuộc publication; giao dịch không có thay đổi nào thì không gửi gì.
- `C`: flags 0, commit_lsn = đầu bản ghi `Tx` cuối, end_lsn = vị trí ngay sau nó, thời điểm.
- `R`: gửi khi bảng xuất hiện lần đầu trong stream, và khi replica identity của bảng khác lần gửi trước (`d` hoặc `f`); namespace `public`; cột khoá có flags 1.
- `I`: `N` + tuple. `U`: [`O` + dòng cũ, khi FULL], `N` + tuple. `D`: `O` + dòng cũ (FULL), hoặc `K` + tuple đủ số cột, chỉ cột khoá có giá trị, còn lại `n`.
- Giá trị text như output text của pgwire. Float8 in như `float8out` của PG: dạng ngắn nhất đọc lại đúng, dùng dạng mũ `d.ddde±XX` khi số mũ thập phân < -4 hoặc ≥ 15. Timestamptz dạng `YYYY-MM-DD HH:MM:SS[.fff]+00`.
- Giá trị nhị phân (`binary 'true'`): int8/float8 8 byte big-endian, bool 1 byte, timestamptz là int64 µs kể từ 2000-01-01, text là các byte nguyên văn.

**NOTIFY**: payload < 8000 byte; giao theo thứ tự commit; khử trùng cặp kênh+payload trong một giao dịch; tối đa 100.000 thông báo chờ cho mỗi listener, tràn thì bỏ và đếm.

## 10. Bộ nghiệm thu cho bản Rust

Bản Rust chỉ được dùng thay bản Go khi qua **toàn bộ** các mục dưới đây, chạy bằng chính công cụ Go hiện có. Đây là điều kiện 1 ("đúng như nhau") của Cổng 3 ở docs/11. Điều kiện về hiệu năng, và việc khi nào mới bắt đầu port, cũng nằm ở docs/11.

| # | Phép thử | Công cụ | Tiêu chí |
|---|---|---|---|
| A1 | Đọc dữ liệu Go | `stasklogdump` (Go) và bản Rust trên cùng thư mục do Go ghi: có long key, tombstone, giao dịch > 4096 op, Move sau GC, `.walz` S2 và zstd, đuôi rách | output giống từng dòng |
| A2 | Go đọc dữ liệu Rust | như A1, thư mục do Rust ghi | giống từng dòng; Go mở được và trả cùng dòng |
| A3 | Checkpoint giống byte | cùng index, cả hai bản ghi checkpoint | file giống từng byte (nếu khác do thứ tự trường JSON: ít nhất A1/A2 phải qua) |
| A4 | Nhân bản chéo | leader Go → replica Rust, và ngược lại; base backup, rồi stream dưới tải | bảng replica trùng từng dòng với leader tại cùng LSN (như `repl_test`) |
| A5 | SQL | `scripts/sqlcorpus.sh` (188 câu, so với PG16 / ClickHouse 25.8) | 188/188 khớp checksum, như bản Go (`results/sqlcorpus/final.md`) |
| A6 | Wire protocol | `psql`, pgx, JDBC qua `internal/pgwire` (extended query, COPY) | các test pgwire của Go chạy với server Rust |
| A7 | Bền vững | `staskchaos` (kill -9, đầy đĩa, fsync EIO) và `scripts/powercut.py` (dm-flakey) | 0 giao dịch đã ACK bị mất, 0 giao dịch thấy một phần |
| A8 | Recovery | 10 GB log (`scripts/rto10g.sh`) | RTO < 5 s |
| A9 | Hiệu năng | các benchmark ở docs/08, docs/06 và docs/10 §4, cấu hình giống hệt | theo tiêu chí chuyển đổi (docs/11) |
| A13 | Trigger | `TestTrigger*` (`internal/sql`), `TestReplicaTrigger` (`internal/repl`): trigger thân SQL chạy trong giao dịch ghi, rollback cùng nhau, bền qua replay và checkpoint, không chạy lại trên replica; bảng hệ thống `__stask_triggers` (docs/05 §20) | toàn bộ PASS với server Rust, cùng dòng trong `pg_trigger` |
| A10 | CDC và NOTIFY | `TestSQLLogicalDecoding`, `TestSQLPublicationsAndIdentity`, `TestCDCHTTP`, `TestListenNotify` chạy với server Rust; `accept/logrepl` và `accept/pgsub.sh` (subscriber PG 16 thật) trỏ vào server Rust; slot do Go tạo được Rust đọc tiếp và ngược lại | chuỗi test_decoding/wal2json giống từng byte; `logrepl` PASS; `pgsub` mọi dòng OK; sau khi đổi bản, stream tiếp đúng sau giao dịch đã xác nhận |
| A11 | Vai trò và quyền | `TestRolesAndGrants`, `TestRolesSurviveRestart` (`internal/sql`), `TestRolesOverWire`, `TestRolesOnlyClosesSharedPassword`, `TestReplicationNeedsPrivilege` (`internal/pgwire`), `TestReplicaRoles` (`internal/repl`) chạy với server Rust, thư mục do Go ghi có bảng `__stask_roles` | cùng kết quả; đọc đúng dòng `__stask_roles` (cột `name` string, `flags` int: bit 0 superuser, 1 login, 2 replication; `verifier` text `SCRAM-SHA-256$iters:salt$stored:server` base64; `grants` text: bản ghi `bits\x01bảng` cách nhau NUL) |
| A12 | Scrub và metrics | `TestScrub`, `TestScrubTailGarbage` (`internal/core`), `TestMetricsAndScrubEndpoints` (`internal/server`) | lật 1 byte trong segment/checkpoint thì báo đúng một lỗi; `/metrics` đúng định dạng Prometheus |
| A15 | Index phụ và ràng buộc | `go test ./internal/core -run 'SecIndex|UniqueIndex|ForeignKey'`, `./internal/sql -run 'SQLIndex|SQLUnique|SQLConcurrentConstraints'`, `./internal/repl -run SecondaryIndex` với server Rust | cùng truy vấn trước/sau `CREATE INDEX` ra cùng dòng và index được đọc; đúng một người thắng khi chèn đồng thời một giá trị UNIQUE; không có con mồ côi khi chèn con đua với xoá cha; replica khởi động lại vẫn đọc được index |

Từ giờ, mọi thay đổi định dạng ở bản Go đều phải tăng số phiên bản (header checkpoint, magic `STZ1`/`STASKREPL1`) và cập nhật tài liệu này trong cùng thay đổi.
