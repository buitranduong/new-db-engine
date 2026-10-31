# 30. Thực thi lai và bộ điều phối truy vấn (thiết kế, 2026-10-31, nhánh `hybrid`)

Đây là tài liệu THIẾT KẾ. Không có dòng mã engine nào được đổi trong nhánh này; chỉ có tài liệu và bộ workload dữ liệu/SQL ở `scripts/workload-multi/`. Mọi con số đo trong tài liệu là số của người khác hoặc của các nhánh trước, được trích đúng như đã đo; những gì thiết kế "kỳ vọng" đều gắn nhãn ước lượng và nằm trong danh sách đo cuối (D32), không ngoại suy.

Nhãn bằng chứng dùng xuyên suốt:
- **[mã]** đã xác minh bằng cách đọc mã trên nhánh `hybrid` (gốc `main` @ `1d73351`), có tên tệp.
- **[đo]** đã xác minh bằng phép đo (ghi nguồn: docs/14, nhánh `r2perf`, hoặc phép chạy trong phiên này).
- **[VL]** từ văn liệu/tài liệu hệ khác. Ba trang được đọc lại trong phiên này (ghi "đọc lại"); phần còn lại là kiến thức đã học, CHƯA tra lại từng câu, vì vậy chỉ dùng để chọn hướng, không dùng làm bằng chứng (AGENT_PROTOCOL §2b).
- **[ƯL]** ước lượng hoặc điều chưa biết; phải đo.

## 0. Tóm tắt một trang

**Vấn đề.** Bài toán là một engine cho 30-100 triệu dòng, nhiều bảng, join phức tạp, đọc phân tích kiểu ClickHouse và giao dịch kiểu PostgreSQL cùng lúc, dưới MỘT log tin cậy, và truy vấn không bao giờ "nhanh nhưng cũ mà không biết" (docs/29 §1). Hiện trạng [mã]:
1. Cột chỉ trả lời một dạng câu: aggregate GROUP BY trên MỘT bảng (`internal/sql/views.go` `viewGroups` → `planner.ViewAggregate` → `proj.Columnar.Aggregate`). View không có API trả dòng, lọc, xếp hạng hay join.
2. Câu R2 (xếp hạng theo nhà mạng có phân trang) và mọi join chạy trên bộ thực thi dòng tổng quát: mỗi bảng được quét hết thành `[]Datum`, join băm trong RAM, build bên phải, không thống kê (`query.go` `tablePlan.run`, `joinPlan.run`; `indexpath.go` ghi rõ "no cost model").
3. Đọc `strong` mà view đang chậm thì CHỜ view (`planner.waitView`, đứng 500 ms hoặc quá 10 s thì quét dòng); không có đường "đọc cột tới LSN A rồi vá phần thay đổi (A, S]".
4. "Bộ điều phối" chỉ tồn tại ở mức HTTP (`planner.Platform.choose`); SQL định tuyến theo cú pháp (`route.go` `needsGeneral`, `buildViewQuery`).

**Thiết kế.** Bốn phần quyết định, mỗi phần có cơ chế, cấu trúc dữ liệu và chủ sở hữu mã (§4):
- (a) **Bộ điều phối lai** (`internal/sql/hybrid_*.go` + `internal/planner`): từ hình dạng câu, hợp đồng nhất quán, trạng thái view/delta và tài nguyên, chọn trên TỪNG bảng một trong KEY / INDEX / COLUMNAR+DELTA / CORE-SCAN và phương pháp join; luôn in lý do, LSN, độ trễ.
- (b) **Chiếu cột dùng được trực tiếp từ SQL**: API quét có lọc, top-N theo nhóm, build/probe join trên lô cột, lấy dòng muộn; thay vì chỉ `Aggregate`.
- (c) **Delta giao dịch**: view là phần "stable" ở LSN A; tập khoá đổi trong (A, S] đọc từ log (không cần cấu trúc đĩa mới); dòng của các khoá đó lấy từ core ở đúng snapshot S, loại các vị trí tương ứng khỏi view. Kết quả chính xác ở S mà không chờ view và không quét core (bổ đề ở §4.3).
- (d) **Cách ly tài nguyên theo lớp công việc** (R, T, B, M), phân lớp theo hợp đồng chứ không theo từ khoá; vì delta tách "độ mới" khỏi "tốc độ áp view", R không phải đợi việc nền.

**Giai đoạn 1** (kiến trúc, không phải bản benchmark): bộ điều phối + delta + top-N theo nhóm từ cột + join sao (dimension broadcast) + EXPLAIN đầy đủ, ở 10 triệu dòng, có cập nhật đồng thời; khác `r2perf` ở chỗ đổi KẾ HOẠCH VẬT LÝ chứ không song song hoá bộ thực thi dòng (§6, §8).

**Điều chưa biết** (§10): tốc độ áp view khi nhập hàng triệu dòng; bộ nhớ của view với khoá chuỗi 50M; chi phí đọc log cho delta lớn; có cần chỉ mục `(category, price)` trên đường R2 hay không; hiệu năng thật của mọi thứ trên.

## 1. Nghiên cứu tiền lệ (làm TRƯỚC, AGENT_PROTOCOL §8)

Câu hỏi chung: mỗi hệ giữ đọc vừa nhanh vừa đúng khi dữ liệu đổi bằng cách nào, hợp đồng nhất quán là gì, STASK dùng lại được gì.

| Hệ | Cách giữ đọc nhanh và đúng | Hợp đồng | Dùng lại cho STASK |
|---|---|---|---|
| **TiDB / TiFlash** | Bản sao cột là Raft learner, đồng bộ bất đồng bộ nên ghi ở TiKV không bị chặn. Mỗi lần đọc kiểm tiến độ sao chép bằng một RPC nhẹ tới leader và chỉ phục vụ khi tiến độ đã phủ read timestamp (đọc lại trang tiflash-overview, [VL] đọc lại). DeltaTree: lớp delta (ghi mới) + lớp stable (đã sắp, bất biến), đọc trộn hai lớp, gộp nền (merge delta) [VL, chưa đọc lại]. Optimizer chọn TiKV, TiFlash hoặc cả hai TRONG MỘT câu theo chi phí dựa trên thống kê (đọc lại) | Snapshot isolation như TiKV (đọc lại) | (1) chờ-tiến-độ đã có (`waitView`) nhưng TiFlash chờ là phương án duy nhất còn ta thêm phương án vá delta; (2) mô hình delta+stable chính là §4.3; (3) chọn engine theo từng truy vấn và từng bảng, không theo từ khoá |
| **SingleStore** | Universal storage: rowstore trong RAM đứng trước columnstore; ghi vào segment dòng, nền chuyển thành segment cột đã sắp theo sort key; segment cột có min/max để bỏ qua (segment elimination); UPDATE = đánh dấu xoá + chèn mới [VL, chưa đọc lại; trang FAQ thử mở trả 404] | Giao dịch ACID trên cả hai dạng; đọc thấy cả hai | Đánh dấu xoá + chèn mới (đã có trong `colTable.put`), "front" dòng nhỏ = delta của ta, min/max theo chunk để bỏ qua (giai đoạn 4, chỉ đáng khi dữ liệu có cụm) |
| **SAP HANA** | Main store (từ điển nén, chỉ đọc) + delta store (tối ưu ghi); truy vấn đọc cả hai, có thao tác "delta merge" nền; hiển thị theo MVCC [VL] | MVCC snapshot | Cùng mẫu với §4.3. Bài học: gộp delta vào main phải sắp lại từ điển; của ta chỉ cần nén chunk, rẻ hơn |
| **HyPer / Umbra** | HyPer: MVCC trong bộ nhớ, cập nhật tại chỗ, undo buffer, vector phiên bản theo chunk; quét nhanh trên chunk không có phiên bản; thực thi song song theo morsel (Leis et al., SIGMOD 2014); biên dịch truy vấn [VL, chưa đọc lại] | Serializable (Neumann et al., SIGMOD 2015) | Cờ "chunk này có khoá bị delta không" cho phép quét nhanh; hàng đợi morsel đã có (`chunkQueue`, work stealing). Biên dịch: không (ngoài phạm vi) |
| **ClickHouse** | Part bất biến + gộp nền. ReplacingMergeTree khử trùng KHI GỘP; `FINAL` khử trùng lúc đọc, tốn thêm; không `FINAL` có thể thấy trùng hoặc dòng đã xoá; tính đúng chỉ là "eventual" (đọc lại trang replacing-merge-tree, [VL] đọc lại). CollapsingMergeTree (cột dấu), lightweight DELETE/UPDATE [VL, chưa đọc lại]. Join: hash (mặc định), parallel_hash, grace_hash, partial_merge, full_sorting_merge, direct [VL]. Projection, chỉ mục thưa, `LIMIT n BY` (top-n theo nhóm) [VL] | Đọc thấy tập part lúc bắt đầu; không giao dịch cập nhật | `LIMIT n BY` chính là toán tử top-N theo nhóm cho R2. Bài học TRÁNH: khử trùng "eventual" không dùng được cho đọc `strong`; `FINAL` cho thấy giá của khử trùng lúc đọc không có chỉ mục khoá→vị trí, mà ta CÓ (`keyPos`/`intPosIndex`) |
| **DuckDB / Velox** | Thực thi vector hoá theo lô nhỏ (khoảng 2048 giá trị), vector chọn (selection vector), vector từ điển, hash join chia radix; DuckDB MVCC cột kiểu HyPer; đẩy bộ lọc từ phía build xuống scan phía probe (join filter pushdown) [VL, chưa đọc lại, chi tiết loại bộ lọc phải kiểm] | Snapshot isolation | Mô hình toán tử theo lô + vector chọn đã nằm trong `colTable.selectChunk`/`filter`; thêm toán tử join, top-N theo nhóm cùng kiểu |
| **Citus columnar** | Lưu theo stripe/chunk group có min/max bỏ qua; bảng cột là append-only: không hỗ trợ UPDATE/DELETE [VL, chưa đọc lại] | không áp dụng | Là ví dụ ĐỂ TRÁNH: cột append-only không phục vụ được đơn hàng/tồn kho. Co-location theo cột phân phối là ý "co-partition" (giai đoạn 4) |
| **Greenplum / Vertica** | Vertica: projection đã sắp và đã chia đoạn (segmentation); WOS (RAM, ghi) + ROS (đĩa, đọc), tuple mover chuyển nền, xoá bằng delete vector, đọc hợp ROS+WOS theo epoch; join đồng vị khi hai projection chia cùng khoá; projection nối sẵn (pre-join). Greenplum: bảng AO/AOCS cột, bitmap xoá, phân phối băm theo khoá [VL, chưa đọc lại] | Snapshot theo epoch | Epoch = LSN của ta; delete vector = `live` bitmap; tuple mover = `maybeCompact`; projection nối sẵn là phương án giai đoạn 4 nếu đo thấy join là nút cổ chai |
| **Apache Doris / StarRocks** | StarRocks Primary Key: Delete+Insert, chỉ mục khoá chính ánh xạ khoá → (rowset, segment, rowid), DelVector đánh dấu xoá theo segment; vì bản cũ đã bị đánh dấu xoá lúc ghi nên truy vấn chỉ đọc bản mới nhất, KHÔNG cần trộn lúc đọc (đọc lại trang primary_key_table, [VL] đọc lại); công bố nhanh hơn 3-10 lần bảng Unique Key kiểu Merge-on-Read. Doris Unique Key merge-on-write dùng delete bitmap tương tự; runtime filter (bloom, min/max, IN) từ phía build; colocate join; materialized view [VL, chưa đọc lại] | Công bố đọc theo phiên bản đã commit | View cột của ta ĐÃ là mô hình này ở phía đọc (live bitmap + chỉ mục khoá). Khác biệt then chốt: Doris/StarRocks duy trì nó ĐỒNG BỘ lúc ghi nên không có "độ trễ"; ta áp BẤT ĐỒNG BỘ nên cần delta. Runtime filter lấy cho join (§5) |

**Rút ra ba họ giải pháp.** (A) delta + main, đọc trộn (HANA, TiFlash, SingleStore, Vertica): chọn cho STASK vì hợp đồng `strong` đúng mà không chặn ghi. (B) duy trì đồng bộ bằng chỉ mục khoá + delete vector, không trộn (StarRocks, Doris MoW): đúng nhưng đưa chi phí vào đường ghi; không chọn làm mặc định vì ghi/đơn hàng là việc ưu tiên. (C) khử trùng "eventual" lúc gộp (ClickHouse Replacing): KHÔNG dùng cho `strong`.

**Nói thật về giới hạn tra cứu.** Tôi chỉ đọc lại ba trang trong phiên này (TiFlash overview, ClickHouse replacing-merge-tree, StarRocks primary_key_table). Trang DeltaTree không nằm trong trang TiFlash overview được đọc; trang SingleStore thử mở trả 404. Mọi dòng gắn [VL, chưa đọc lại] cần agent kế tiếp tra lại nguồn gốc (bài báo VLDB 2020 "TiDB: a Raft-based HTAP database", SIGMOD 2015 HyPer MVCC, SIGMOD 2014 morsel-driven, tài liệu Vertica/Doris) trước khi dựa vào chi tiết. Hướng chọn không phụ thuộc chi tiết đó.

## 2. Bản đồ các đường đọc hiện tại [mã]

| # | Đường | Điểm vào | Khi nào | Giới hạn |
|---|---|---|---|---|
| P0 | Đọc theo khoá | `exec.go` `runHere` → `selectRows` (≈ dòng 1412) → `keyLookups`, `core.Tx.Get` | WHERE là khoá chính | chỉ một bảng |
| P1 | Chỉ mục phụ, khoảng khoá | `indexpath.go` `chooseIndex`, `boundsOf` | tiền tố bằng + khoảng; `ORDER BY ... LIMIT` theo thứ tự chỉ mục | KHÔNG mô hình chi phí và KHÔNG thống kê: "any usable index wins over a full scan" (chú thích đầu `indexpath.go`) |
| P2 | Trigram/tìm kiếm | `internal/proj/substr.go`, `search.go` | LIKE chứa/hậu tố | một bảng |
| P3 | Aggregate trên view cột | `views.go` `viewGroups`; `planner.ViewAggregate`; `proj.Columnar.Aggregate` | một bảng `WITH (columnar=true)`, GROUP BY cột, count/sum/avg/min/max, WHERE là hội/tuyển của so sánh cột-hằng; autocommit (không trong BEGIN) | không trả dòng; không join; không cửa sổ; không biểu thức (`buildViewQuery`, `conjFilters`) |
| P4 | Quét một bảng, tính trong Datum | `exec.go` `selectRows` | mọi SELECT một bảng còn lại | cả bảng vào Datum; vượt ngân sách → `errSimpleSpill` → bộ thực thi tổng quát |
| P5 | Bộ thực thi tổng quát | `route.go` `needsGeneral` → `query.go` `runQuery`, `compileSelect`, `tablePlan.run`, `joinPlan.run`, `window.go`, `semijoin.go`, `supfix_scan.go` `runParallel` | join, cửa sổ, CTE, subquery, `LIMIT BY`, tập hợp | MỖI bảng quét hết vào `[]Datum`; join băm theo khoá chuỗi (`groupKey`), build bên phải, trái→phải; `joinorder.go` chỉ đổi thứ tự cho danh sách FROM dấu phẩy; WHERE chỉ được đẩy xuống thành điều kiện join (`pushDown`), KHÔNG đẩy thành bộ lọc quét từng bảng (trên `main`; `r2perf` thêm lọc ở bước quét cho truy vấn một bảng) |
| P6 | Spill | `spillexec.go`, `internal/spill` | gặp 53200 và `-spill` | chạy lại truy vấn một lần; không cửa sổ/`LIMIT BY` |
| P7 | AS OF / lịch sử | `asof.go`, `core/history.go` | `AS OF`, `stask_history` | đọc theo khoá |

**Bộ điều phối hiện có.** `internal/planner/planner.go` có `Consistency{strong,bounded,session,eventual}`, `choose()` chọn view hay core và trả `Plan{Engine, Mode, Reason, DurableLSN, AppliedLSN, LagMs, WaitedMs}`, nhưng chỉ dùng cho API HTTP (`Aggregate`, `TimeSeries`, `Search`). SQL chỉ gọi `ViewAggregate` và `ViewInfo`: không có hợp đồng ở mức câu lệnh ngoài `stask.read_staleness` [mã].

**Độ mới của view.** `core.Follower` (`follow.go`) đọc các bản ghi DURABLE từ file log (`log.Scan`), áp vào `Consumer.Apply` (xếp hàng `pending`), `Flush` khi `advance`, công bố `applied` mỗi 16 384 thao tác hoặc 20 ms. `Lag()` = tuổi commit kế tiếp chưa áp (`engine.go` `LagSince`, vòng `tsRing`). `Flush(uint64)` bỏ qua tham số lsn [mã]. Số đo: trễ view p50 41 ms, p99 208 ms ở ghi nền đều ~2 000/s, sau tối ưu p99 52 ms (docs/04, docs/15) [đo, nguồn cũ]. KHÔNG có số đo tốc độ áp view khi nhập hàng triệu dòng [ƯL].

**Cách một truy vấn `strong` chờ.** `waitView` (`planner.go`): chờ `WaitApplied(snapshot)` từng nhịp `ViewStall`=500 ms; nếu `applied` không tiến sau một nhịp, hoặc quá `ViewMaxWait`=10 s, trả false và câu lệnh quét dòng [mã]. `stask.read_staleness` cho phép đọc view trễ tới mức đó không chờ [mã]. Câu trong `BEGIN` không dùng view vì view có thể chứa commit muộn hơn snapshot [mã `views.go`].

**View lưu gì** (`internal/proj/columnar.go`, `colvec.go`, `colstore.go`) [mã]:
- Chỉ trạng thái MỚI NHẤT mỗi khoá, không có phiên bản. `put` = xoá bit `live` của vị trí cũ (`dead++`), thêm dòng mới ở cuối; `maybeCompact` viết lại khi `dead ≥ 65 536` và ≥ một nửa.
- Cột theo chunk 65 536 dòng (`chunkRows`); cột số nguyên nén frame-of-reference 1/2/4 byte (`pvec`); chuỗi mã hoá TỪ ĐIỂN RIÊNG THEO CỘT, mã `uint32`; bitmap `nulls` và `live`; numeric(p≤18) lưu thành số nguyên nhân 10^scale; double lưu thô.
- Cột TEXT vượt `textDictMax` = 65 536 giá trị khác nhau bị BỎ khỏi view ("free text"). Hệ quả cho SIM: cột `number` (hàng chục triệu giá trị) không có trong view; truy vấn cần nó "quét dòng".
- Khoá: khoá nguyên → `intPosIndex` (băm mở trên chính cột khoá); khoá chuỗi → `keyPos map[string]uint32` (map Go). Không có truy cập vị trí → khoá ngoài việc dựng lại map (xem `maybeCompact`, `keyOf`). Bộ nhớ map này ở 50M khoá chuỗi CHƯA ĐO [ƯL]. View lưu ra file có phần `keysSection` (`colstore.go`), nén zstd.
- `Aggregate` giữ `ct.mu.RLock()` SUỐT lần quét (`columnar.go:566`), còn `Flush` cần `ct.mu.Lock()`. Hệ quả [mã, suy luận từ cách `sync.RWMutex` hoạt động]: một lần quét dài chặn việc áp view, và writer đang chờ làm các reader mới phải xếp sau nó. Đây là đảo ưu tiên giữa quét nặng và truy vấn ngắn trên cùng bảng; chưa có số đo.
- Bỏ qua chunk bằng min/max: CHỈ dùng cho khoảng dày của khoá nhóm (`denseRange`); bộ lọc (`filter`) duyệt mọi chunk [mã].
- Đọc bảng nhiều bảng: mỗi `colTable` có khoá riêng, `Flush` áp tuần tự từng bảng, nên một truy vấn nhiều bảng không thấy một LSN duy nhất trên cả hai bảng [mã]. (Hiện chưa lộ ra vì view chỉ phục vụ một bảng.)

**Join** [mã]: xem P5. Không chỉ mục nested-loop trong bộ thực thi tổng quát (chỉ `semijoin.go` băm cho EXISTS/IN tương quan), không thống kê, không bộ lọc runtime, không broadcast.

**Ngân sách bộ nhớ** [mã]: `membudget.go` tính từng dòng Datum vào `-query-mem-mb` và `-sql-mem-mb`, vượt thì 53200. LỖI ĐÃ BIẾT: trên `main` và trên nhánh này `tablePlan.runParallel` (`supfix_scan.go:82`, `_ = over.Load()`) bỏ qua cờ vượt ngân sách và TRẢ MỘT TIỀN TỐ CỦA BẢNG như kết quả (quét song song 10M dòng thiếu 14% bảng, kết quả sai im lặng). Đã tìm ra và sửa ở `r2perf` (commit `7091a73`, `TestParallelScanOverBudgetIsAnError`), CHƯA vào `main` [đo, docs/14 Phụ lục U r2perf]. Điều kiện cứng của thiết kế này (§4.6, I2): mọi toán tử mới thất bại to tiếng (53200/57014), không bao giờ trả kết quả một phần.

**Làn** [mã] (`lanes.go`, `laneagg.go`, `admission.go`): `heavy()` phân loại theo hình dạng; SELECT có LIMIT bị coi là "bounded", chuyển làn nặng nếu trung bình động > `slowSelect` = 150 ms (`noteSelectTime`); aggregate view ≤ `ViewNormalRows` = 2M dòng và view đủ mới ở làn thường; còn lại làn nặng (luồng OS riêng, token bucket `-heavy-share`). Câu R2 dạng `SELECT ... FROM (subquery)` có `From==""` nên vào làn thường; `r2perf` thêm test `TestR2QuotaQueryStaysOnNormalLane` [đo]. Rủi ro còn lại: một biến thể R2 viết khác (CTE, một bảng có LIMIT chậm > 150 ms) có thể bị hạ làn vì phân loại theo hình dạng + thời gian, không theo hợp đồng; chưa kiểm [ƯL].

**Số đo R2 đã có** (`r2perf`, chỉ báo, VM chung tải 2,5-8): 10M, trang 1: 23,1 s (trước) → 14,9 → 7,4 → **5,3 s**; PostgreSQL 16 cùng dữ liệu 6,4 s; ClickHouse 0,9 s (lần chạy trước, không đo lại); 1M: STASK 537 ms, PG 1 219 ms, ClickHouse 110 ms. Truy vấn KHÔNG dùng view cột cũng không dùng chỉ mục `(category, price)`; còn vật hoá 1M dòng khớp và sắp xếp chúng. Nhánh dừng sau hai vòng (D35) và ghi rõ bước kế: top-N theo phân vùng từ dữ liệu cột [đo].

## 3. Nguyên tắc thiết kế

1. Một log là sự thật; mọi cấu trúc đọc là dẫn xuất và dựng lại được (docs/01 I4). Thiết kế KHÔNG thêm cấu trúc bền mới ở các giai đoạn 1-3.
2. Mỗi câu trả lời có hợp đồng và chứng minh được: hoặc "phản ánh đúng snapshot S", hoặc "trễ không quá B ms, đang trễ X ms", và EXPLAIN in chứng minh đó.
3. Chọn đường theo hình dạng + hợp đồng + trạng thái, NHƯNG chọn bằng luật và số đếm thực (số dòng sống, kích thước delta, kích thước từ điển, kết quả thực thi trước phía nhỏ), không dựa vào bảng thống kê chưa tồn tại.
4. Thất bại to tiếng: không toán tử nào được trả tiền tố, mẫu hay "gần đúng" khi hết ngân sách, huỷ, hết giờ; bậc thang lùi (xuống đường chậm hơn nhưng ĐÚNG) hoặc lỗi.
5. Đóng băng tương thích (người điều phối): không thêm tính năng tương thích PostgreSQL trừ khi một câu của workload §7 cần.

## 4. Thiết kế: bốn phần quyết định

### 4.1 (a) Bộ điều phối lai

**Chủ sở hữu mã.** Phân tích hình dạng nằm trong `internal/sql` vì cần AST: `hybrid_shape.go` (rút `Shape` từ `*Select`), `hybrid_plan.go` (quy tắc chọn, cây kế hoạch), `hybrid_explain.go`, `hybrid_exec.go` (chạy). Trạng thái view/delta và hợp đồng nằm ở `internal/planner` (mở rộng `Platform`, KHÔNG sửa `choose` đang dùng cho HTTP). Giao diện: `internal/sql` định nghĩa interface `Hybrid` (cách `Views` đã làm để tránh vòng import) và `planner.Platform` cài đặt. Chỉ móc vào mã cũ ở ba chỗ, mỗi chỗ vài dòng: `exec.go runHere` (nhánh `*Select`, trước `needsGeneral`), `explain.go` (cạnh `viewAggNode`), `lanes.go heavy()` (phân lớp, §4.4).

**Đầu vào.**
- `Shape`: các bảng và bí danh; đồ thị join (cạnh bằng, loại); với mỗi bảng, các vị từ phân loại *sargable-trên-cột* (so sánh/IN/BETWEEN/IS NULL như `conjFilters`), *sargable-trên-chỉ-mục*, *còn lại*; GROUP BY/aggregate; mẫu "top-N theo nhóm" (hàm cửa sổ `row_number/rank` có PARTITION BY + vị từ ngoài trên rn, hoặc `LIMIT n BY`); LIMIT/OFFSET; `count(*) OVER ()`; cột nào được đọc.
- `Contract` từ GUC `stask.consistency` = `strong` (mặc định) | `bounded` (kèm `stask.max_lag`, hoặc bí danh cũ `stask.read_staleness`) | `session` (kèm LSN token, tái dùng nhánh `session` của `planner.Consistency`) | `eventual`. Không có GUC → `strong`, như hôm nay.
- Trạng thái: cho từng bảng cột: `Ready`, `applied` (cận dưới/trên, §4.3), `Lag`, số dòng sống, kích thước từ điển từng cột, cột nào KHÔNG có trong view; kích thước delta ước lượng từ vị trí log (`DurablePoint` trừ vị trí của follower, đều là số đã có [mã]); tốc độ áp view (trung bình động đo ở `Follower`; mới); bộ nhớ ngân sách còn; lớp tải (§4.4).

**Đầu ra.** Cây kế hoạch, mỗi lá là một đường truy cập: `KEY | INDEX(tên) | COLUMNAR+DELTA | CORE-SCAN`; mỗi nút join có phương pháp (`DENSE-BROADCAST | HASH(build=...,runtime-filter) | INDEX-NL(chỉ mục) | SEMI/ANTI`); nút gốc có "bằng chứng hợp đồng" (S, A, lag).

**Luật chọn (theo thứ tự, dừng ở luật đầu thoả).**
| # | Điều kiện | Chọn |
|---|---|---|
| R1 | Vị từ khoá chính hoặc ≤ 256 khoá | KEY (giữ nguyên `heavyReadKeys`) |
| R2 | Mọi cột bảng cần có trong view, bảng cột, tự commit hoặc ở REPEATABLE READ (§4.3 cho phép), hình dạng thuộc tập toán tử cột | COLUMNAR+DELTA nếu hợp đồng thoả (bảng dưới) |
| R3 | Có chỉ mục phủ khoảng và ước lượng số khoá ≤ 2% số dòng sống (số đếm thực từ cận chỉ mục, không thống kê) | INDEX |
| R4 | còn lại | CORE-SCAN (bộ thực thi tổng quát, có ngân sách + spill) |

Thoả hợp đồng cho COLUMNAR+DELTA:
| Hợp đồng | Điều kiện | Nếu không |
|---|---|---|
| strong | delta ước lượng ≤ `HybridDeltaMax` (khởi điểm 100 000 khoá, ƯL, chỉnh bằng đo) → vá delta; nếu lớn hơn và view đang tiến với tốc độ đo được sao cho dự kiến kịp trong `HybridWaitMax` (khởi điểm 200 ms) → chờ; nếu không → R3/R4 | CORE-SCAN, in lý do |
| bounded B | `Lag ≤ B` → dùng nguyên không vá (nhanh nhất); nếu `Lag > B` thì như strong | như strong; hoặc lỗi nếu `SET stask.fallback='error'` |
| session L | `applied ≥ L` → như bounded; nếu không vá delta tới S ≥ L | như strong |
| eventual | dùng nguyên | — |

**Bậc thang lùi** (không bao giờ im lặng, mỗi bước ghi vào EXPLAIN ANALYZE và bộ đếm `/v1/status.hybrid`): COLUMNAR+DELTA → (cột thiếu / delta quá lớn / vượt ngân sách toán tử cột) → INDEX hoặc CORE-SCAN song song → (vượt ngân sách) spill → 53200. Không có bước nào trả kết quả một phần.

**Vì sao không "cost-based đầy đủ".** STASK không có thống kê [mã]. Thiết kế dùng ba nguồn số đếm đã rẻ hoặc sẵn có: số dòng sống, kích thước từ điển từng cột (độ phân biệt), và "thực thi trước phía nhỏ" của join (§5). TiFlash/CockroachDB chọn bằng thống kê [VL]; việc thêm thống kê là backlog nếu đo cho thấy luật sai (giai đoạn 4).

### 4.2 (b) Chiếu cột dùng trực tiếp từ SQL

Hôm nay chỉ có `Columnar.Aggregate`. Thêm vào `internal/proj` (tệp mới: `colscan.go`, `colrank.go`, `coljoin.go`, `colkeys.go`; sửa `columnar.go` tối thiểu):

1. **`Scan(tableID, ScanSpec) Batches`**: `ScanSpec{Cols, Filters, Exclude, Workers, Spawn, Keep, Interrupted}`; trả luồng lô `Batch{Table, Chunk, Sel []uint32, Col(i) accessor}`. Dùng lại `selectChunk`/`filter`/`chunkQueue`/work stealing đang có. `Exclude` là danh sách vị trí đã sắp (từ delta) áp ở bước chọn.
2. **Cột khoá (`colkeys.go`)**: arena khoá cho vị trí → khoá, để (i) xếp hạng theo khoá/hàm của khoá (R2: `md5(number||'42')`), (ii) lấy dòng muộn từ core. Thay `keyPos map[string]uint32` bằng bảng băm mở trên arena (như `intPosIndex`) để giảm bộ nhớ. Đổi nội bộ trong RAM; file view đã có `keysSection`, không đổi định dạng [ƯL: cần xác nhận khi cài]. Bộ nhớ ước 50M × (11 B khoá + 4 B offset + bảng băm) ≈ vài trăm MB đến ~1 GB [ƯL, phải đo; map Go hiện tại chưa đo].
3. **`TopNPerGroup(spec)`**: nhóm theo cột mã từ điển (`carrier_id`), khoá xếp thứ tự là danh sách `(cột hoặc biểu thức, hướng)`, `K[g]` theo nhóm. Mỗi worker giữ heap kích thước `K[g]` mỗi nhóm; **cắt tỉa theo khoá dẫn đầu có độ phân biệt thấp**: khi heap nhóm đã đầy, một dòng có khoá dẫn đầu (ví dụ `store_rate`) kém ngưỡng bị loại KHÔNG cần tính biểu thức đắt (md5); chỉ các dòng cùng bậc khoá dẫn đầu với ngưỡng mới tính khoá đầy đủ. Biểu thức sắp xếp được tính bằng bộ đánh giá biểu thức SQL hiện có (`evalCtx`) trên dòng dựng lại từ cột + khoá, chỉ cho các ứng viên. Gộp heap các worker; áp dụng ngưỡng rn ngoài (cận dưới bỏ qua, cận trên là K). Chỉ nhận khi so sánh văn bản là bytewise (cùng điều kiện `Coll.Bytewise()` của `conjFilters`), nếu không → R4. Tiền lệ: `LIMIT n BY` của ClickHouse [VL]; tiền lệ nội bộ: top-k không cấp phát trong `Aggregate` (docs/05 §8).
4. **Join trên lô** (`coljoin.go`): xem §5.
5. **`FetchRows(keys)`**: lấy cột ngoài view và cột kết quả bằng `core.Tx.Get` song song (đọc muộn). Với R2: 100 dòng.
6. **Đảm bảo nhất quán nhiều bảng**: mỗi `colTable` ghi `appliedLSN` trong `Flush(lsn)` (hôm nay tham số bị bỏ qua) và `Columnar` công bố cận trên `applyingTo` TRƯỚC khi chạm bảng (§4.3).

Không làm ở giai đoạn 1: cột chuỗi dạng phẳng cho văn bản nhiều giá trị (ngoài khoá), sắp theo sort key, zone map cho bộ lọc, từ điển dùng chung giữa các bảng. Lý do từ điển dùng chung: join qua mã từ điển cần bảng dịch mã (mảng `[]uint32` cỡ ≤ 65 536 dựng một lần mỗi truy vấn, O(độ phân biệt)); tiết kiệm của từ điển dùng chung nhỏ hơn rủi ro đổi định dạng. Sẽ xem lại nếu đo cho thấy chi phí dịch mã đáng kể [ƯL].

### 4.3 (c) Delta giao dịch: trộn lúc đọc

**Ý tưởng.** View = phần "stable" phản ánh LOG tới LSN A (đúng: ≥ `lo` và ≤ `hi`, xem dưới). Một đọc ở snapshot S cần kết quả chính xác ở S. Tập khoá `K` = các khoá có bản ghi log với LSN trong `(min(lo,S), max(hi,S)]`. Câu trả lời = kết quả toán tử trên các dòng của view NGOẠI TRỪ khoá thuộc K, hợp với các dòng của K đọc từ core ở snapshot S (`core.GetAt`, qua `Tx.Get` của chính giao dịch).

**Bổ đề (đúng đắn).** Với khoá k ∉ K, phiên bản nhìn thấy ở S bằng phiên bản ở mọi LSN trong `[min(lo,S), max(hi,S)]` (không có bản ghi nào đổi k trong khoảng), nên dòng của k trong view (phản ánh một LSN trong `[lo,hi]`) bằng dòng ở S. Với k ∈ K, dòng ở S đọc thẳng từ core là đúng theo định nghĩa của MVCC của core. Hai tập này phủ mọi khoá còn sống ở S. Hệ quả: kết quả chính xác ở S, kể cả khi S < A (giao dịch REPEATABLE READ hoặc token phiên cũ), nên R2 không còn buộc "không dùng view trong BEGIN". [Chứng minh phi hình thức; cần test thuộc tính, §6.]

**Cận `lo`, `hi`.** `lo` = `Columnar.applied` đọc TRƯỚC khi lấy khoá bảng; `hi` = `Columnar.applyingTo` đọc SAU khi thao tác xong (công bố trước khi `Flush` chạm bảng đầu tiên). Vì `applied` đơn điệu, `lo ≤ LSN thật ≤ hi`. Nhờ vậy cũng xử lý được việc `Flush` áp tuần tự từng bảng (§2): mỗi bảng được xét theo cận của nó, hoặc dùng chung cận rộng nhất của lần đọc.

**Nguồn K.** Quét bản ghi log từ vị trí `Follower.pos` (đã có, `follow.go`) tới `DurablePoint`; lọc theo bảng; chỉ lấy khoá và LSN (không giải mã dòng). Bộ nhớ đệm `deltaTail` dùng chung giữa các truy vấn đồng thời (khoá → LSN mới nhất), cắt khi view tiến. Không đổi định dạng đĩa, không bản ghi mới [ƯL: tốc độ quét log cho delta lớn chưa đo; log nằm trong page cache nên kỳ vọng nhanh; số đo bắt buộc ở G3]. Nếu đo cho thấy chậm: vòng đệm thay đổi gần đây trong engine (bộ nhớ, ghi lúc publish durable) là phương án; vẫn không đổi định dạng đĩa.

**Chi phí.** `|K|` lần `GetAt` (mỗi lần ~ đọc điểm, hàng trăm ns đến vài µs [ƯL]) cộng danh sách loại trừ (tra `keyPos`/`intPosIndex` mỗi khoá → vị trí → danh sách đã sắp áp trong `filter`). Chi phí tỉ lệ với delta, không với bảng. Quy tắc §4.1 giới hạn: delta lớn hơn ngưỡng thì chờ hoặc rơi về đường khác, có in lý do.

**Các toán tử trộn delta thế nào.** Dòng của K đi vào CÙNG toán tử như một "lô delta" thêm vào luồng lô cột: lọc trước (cùng vị từ), rồi vào heap top-N (§4.2.3), bảng build/probe join, bộ tích luỹ aggregate (gộp từng phần: `count/sum/min/max` gộp được; `avg` = sum/count; `count(distinct)` cần tập — dùng đường cũ nếu delta không rỗng ở giai đoạn 1, ghi rõ).

**Khi nào lùi.** Cột không có trong view; K vượt ngưỡng; `Compaction` đổi chỉ số vị trí giữa chừng (bảo vệ bằng giữ khoá đọc hoặc phiên bản `epoch` của mảng; xem §4.4 về khoá).

### 4.4 (d) Cách ly tài nguyên

**Lớp công việc** (phân theo hợp đồng/GUC, không theo từ khoá):
- **T** giao dịch ngắn và đọc điểm (đơn hàng, tồn kho, R1).
- **R** truy vấn tương tác có hợp đồng thời gian: R2, trang danh sách, facet. Nhận diện: GUC `stask.workload = 'interactive'` (đặt theo vai trò: `ALTER ROLE web SET ...`, tiền lệ PostgreSQL) hoặc suy ra khi có LIMIT ≤ 1000 và hình dạng thuộc tập toán tử lai. R2 KHÔNG BAO GIỜ bị hạ làn theo thời gian chạy (bỏ `noteSelectTime` cho lớp R); nếu R chậm hơn ngân sách thời gian thì cảnh báo và đếm (`/v1/status.hybrid.slow_r`), không hạ.
- **B** nhập hàng loạt, aggregate lớn, báo cáo, quét toàn bảng.
- **M** nền: áp view, gộp/nén, lưu view (`Save`), GC.

**Cơ chế** (chủ yếu tái dùng): `admission.go` (đọc không chờ slot); `heavyGov` (token bucket `-heavy-share`); `keepHelper` (người giúp nhường khi scheduler bận). Thêm: (1) **hồ người giúp dành cho R**: tối thiểu `HybridRHelpers` (khởi điểm 2) luồng giúp mà B không chiếm được; (2) **B nhường M trước nhưng M có sàn**: áp view cần được bảo đảm tiến bộ để delta không phình, nên M có phần CPU tối thiểu cấu hình được; (3) **bỏ khoá đọc suốt lần quét** (giai đoạn 3): quét bảng không giữ `ct.mu.RLock` cả lần; thay bằng chụp con trỏ chunk + `n` + bản `live` nguyên tử và dựa vào bổ đề §4.3 (cận `hi` đọc SAU lần quét) để vẫn chính xác; `maybeCompact` tạo mảng mới còn người đọc cũ giữ mảng cũ (epoch). Lý do: loại đảo ưu tiên giữa B và R/M (§2); (4) bộ nhớ: ngân sách toán tử cột tính vào cùng `qexec` (`membudget.go`).

**Điểm mấu chốt.** Vì delta cho phép R đọc đúng KHÔNG cần view đuổi kịp, việc nhập hàng triệu dòng chỉ làm delta lớn (R chuyển sang chờ/đường khác có in lý do), không làm R chờ vô hạn hoặc đọc sai. Cách ly CPU là bảo đảm độ trễ, không phải bảo đảm tính đúng.

**Chưa biết:** có cần ràng buộc CPU cứng (cgroup, đã có `cgroup.go`) hay token bucket user-space đủ; có cần cách ly IO (spill, `Save`) — đo ở G3 (docs/24 §3 mục 5).

### 4.5 (e) Hợp đồng EXPLAIN (điều kiện bắt buộc)

EXPLAIN cho truy vấn lai phải nói: đường nào, vì sao, đọc bao nhiêu dòng/byte từ core, delta, cột; chỉ mục nào; LSN; trễ view. Kế hoạch chỉ là "Seq Scan + sort" thì KHÔNG tính là đường lai hoạt động (tiêu chí nghiệm thu §6). Các con số trong khối dưới đây chỉ minh hoạ định dạng. Định dạng chính xác (mỗi dòng là một dòng kết quả của EXPLAIN văn bản, thụt đầu dòng như `explainer.node/detail` hiện có):

```
Hybrid Plan  (contract: strong; snapshot lsn=1204511; view applied lsn=1204470 (lag 38 ms); delta 41 keys)
  -> Top-N per Partition on sims  [rows kept per partition: 30 / 50 / 20]
       Partition: carrier_id    Order: store_rate DESC, md5(number || '42'), number
       Path: COLUMNAR+DELTA
       Why: window row_number() + outer filter on rn recognised; every column read is in the view (carrier_id, category, price, status, store_rate; key arena for number); strong contract met by view@1204470 + delta(41 keys <= 100000)
       Rejected: INDEX (category, price): 1,000,000 of 10,000,000 keys in range (10% > 2%); CORE-SCAN: 10M rows to Datum, no top-N operator
       Read columnar: 10,000,000 rows scanned (153 chunks), 702,113 passed filter, 117,008 order keys evaluated, 3 columns, 140.2 MB touched
       Read delta:    41 keys from log (3.1 KB), 41 view rows excluded, 12 rows added after filter
       Read core:     100 rows fetched by key (24 KB)
       Index:         none
       Memory: 3.2 MB of 2048 MB; lane: R (helpers 4 of 8)
```
Với join, mỗi bảng một nút `Columnar Scan on <t>` cùng khối `Path/Why/Rejected/Read` và nút `Hash Join (DENSE-BROADCAST build=stores 40 rows)`/`Index NL`; dòng cuối nút gốc: `Planning: shape=..., coordinator 0.4 ms`. Khi không dùng đường lai: `Hybrid not used: <lý do>` (như `Columnar view not used` hiện có, `explain.go:429`). `EXPLAIN` thường in ước lượng (kích thước delta, số khoá); `EXPLAIN ANALYZE` in số thật từ bộ đếm nguyên tử của từng toán tử (cùng cấu trúc `hybridStats`, cũng xuất ở `/v1/status` và nhật ký truy vấn chậm).
**Điểm mã:** `hybrid_explain.go` (`explainHybrid`) gọi từ `explain.go` cạnh `viewAggNode`; bộ đếm do `hybrid_exec.go` và các toán tử `proj` điền. Nghiệm thu bằng test khẳng định chuỗi (khuôn mẫu, không số) và so các số với bộ đếm độc lập.

### 4.6 (f) Chế độ hỏng và bất biến

| # | Bất biến | Cách giữ | Test |
|---|---|---|---|
| I1 | Câu `strong` phản ánh mọi commit đã báo xong trước khi bắt đầu (S = durable LSN lúc bắt đầu) | bổ đề §4.3; S lấy từ `tx.Snapshot()` | "ack-rồi-đọc": 10 000 lần ghi xong rồi đọc R2 từ kết nối khác, phải thấy |
| I2 | Không kết quả một phần | mọi toán tử kiểm `Interrupted` theo chunk, ngân sách → lỗi; sửa lỗi `runParallel` (r2perf `7091a73`) là điều kiện tiên quyết | test chèn lỗi: ép ngân sách nhỏ/hủy giữa chừng, so với bảng gốc |
| I3 | Sau sập/khôi phục: view dẫn xuất, tải lại từ file lưu rồi áp lại log (`tryRestore`) hoặc dựng lại; delta suy ra từ log nên không có trạng thái mất | không đổi đường khôi phục | kill -9 lặp (`accept/kill9`) rồi chạy bộ truy vấn §7 |
| I4 | Độ trễ nhìn thấy được | EXPLAIN, `/metrics` (gauge `view_lag_ms`, `delta_keys`), `/v1/status.hybrid` | test chuỗi/đếm |
| I5 | Cái gì bị từ chối bằng lỗi | `strong` mà không chứng minh được và core vượt ngân sách → 53200 kèm gợi ý; `bounded` + `fallback='error'` mà lag > B sau thời hạn → lỗi (mã SQLSTATE chốt ở giai đoạn 1 theo tiền lệ PostgreSQL, ứng viên `55000`) | test |
| I6 | Hai bảng trong một truy vấn nhất quán trên cùng S | cận `lo/hi` chung (§4.3) | test join hai bảng trong lúc giao dịch hai bảng chạy: bất biến q13 |

**Định dạng đĩa (D33).** Giai đoạn 1-3: KHÔNG đổi định dạng đĩa (delta đọc từ log; cột khoá và bảng băm khoá chỉ ở RAM; view lưu `keysSection` đã có). Cần ghi chú: nếu cần thêm phần (section) mới vào file view (thống kê chunk, cột chuỗi phẳng) thì phải kiểm bản cũ có bỏ qua tag lạ không (chưa kiểm [ƯL]); view là dữ liệu dẫn xuất nên xấu nhất là bị loại và dựng lại. Giai đoạn 4 (sort key, co-partition, projection nối sẵn) cần tuỳ chọn bảng mới trong catalog (JSON, `omitempty`, cộng thêm).

## 5. Chiến lược join (d)

Mục tiêu: join nhiều bảng nhanh trên chiếu cột và trên kho dòng; chọn theo số đếm thực.

1. **Dimension broadcast (carriers, stores, customers nhỏ).** Bảng nhỏ (số dòng sống ≤ `BroadcastMax`, khởi điểm 1M, ƯL) được lọc rồi build thành mảng đánh chỉ số theo khoá nguyên (đường "dense", cùng ý với `aggregateDense`) hoặc bảng băm mở trên khoá; quét bảng lớn theo lô, tra khoá cả lô, ra vector chọn + cột payload. Không materialise Datum.
2. **Thực thi trước phía nhỏ (adaptive build).** Với join hai bảng lớn (orders ⨝ order_items): chạy trước bộ lọc cột của phía có vị từ chọn lọc nhất (đếm thật, mất vài chục ms ở 10M [ƯL]) để biết kích thước build. Nếu build ≤ 1% bảng probe và probe có chỉ mục trên khoá join → **INDEX-NL** (tra chỉ mục core cho từng khoá build, `IN`-list); ngược lại **HASH** (build nhỏ hơn, chia radix như `aggregateDense`/`distinct_fast.go`) và **bộ lọc runtime** (min/max + bloom của khoá build) đẩy xuống quét probe để bỏ dòng sớm (tiền lệ: Doris/StarRocks runtime filter, DuckDB join filter pushdown [VL, chưa đọc lại]).
3. **Semi/anti join** (`EXISTS`, `NOT EXISTS`, `IN`): tập khoá (bitset nếu khoá dày, băm nếu không); tái dùng ngữ nghĩa NULL của `semijoin.go` (NULL không bao giờ khớp; `NOT IN` ba trị).
4. **Aggregate sau join** trên lô: khoá nhóm và đầu vào aggregate gom từ payload; dùng lại bộ tích luỹ dày/radix của `proj/agg.go`.
5. **Đọc muộn** cột ngoài view qua `FetchRows`.
6. **Thứ tự join:** thực thi trước phía nhỏ thay cho thống kê; bảng dim nhỏ luôn build; còn lại build = phía có số dòng lọc nhỏ hơn đo được. Join ngoài (LEFT) giữ bên preserved làm probe.
7. **Hết bộ nhớ:** build vượt ngân sách → lùi sang bộ thực thi tổng quát có spill (grace hash join hiện có `spillexec.go`) hoặc 53200; không bao giờ cắt.
8. **Phạm vi:** inner/left/semi/anti equi-join; join không phải bằng → R4. Không thêm tính năng tương thích mới (đóng băng).

## 6. Kế hoạch theo giai đoạn

**Thứ tự hợp nhất với các nhánh lớn (điều kiện 4).** `r2perf` (6 tệp trong `internal/sql`: `query.go` +43, `supfix_scan.go`, `window.go`, `winpar.go` mới, test) và `integ20` (≈ 37 682 dòng thêm, 142 tệp; phần lớn là tệp MỚI `fn_*.go`, `tsearch.go`, `interval.go`, `rangetypes.go`, `exttypes.go`, golden; sửa tệp có sẵn: `exec.go` +147, `query.go` +97, `parse.go`, `explain.go`, `partition.go`, `copy.go`, `merge.go`, `core/fk.go`...) phải vào `main` TRƯỚC. Giai đoạn 1 bắt đầu từ `main` sau hai lần gộp đó (rebase một lần, không làm song song trên `exec.go`/`query.go`). Quy tắc sở hữu của giai đoạn 1: chỉ TẠO tệp mới: `internal/sql/hybrid_*.go`, `internal/proj/col{scan,rank,join,keys}.go`, `internal/planner/hybrid.go`, `internal/core/deltatail.go` (đọc log lấy khoá); chỉ SỬA có sẵn ở ba móc ≤ 15 dòng mỗi nơi (`exec.go` nhánh Select, `explain.go`, `lanes.go heavy()`) cộng `proj/columnar.go` (`Flush(lsn)` ghi `appliedLSN`, `applyingTo`). Theo AGENT_PROTOCOL §3: `internal/proj` một nhánh một lúc; `internal/sql` tối đa 3 nhánh đồng thời nên giai đoạn 1 chạy khi `compat-*` đã gộp.

**Giai đoạn 0 (điều kiện, ~0,5 ngày).** Gộp `r2perf` (có sửa lỗi cắt cụt `7091a73`). Nghiệm thu: `TestParallelScanOverBudgetIsAnError` xanh trên `main`.

**Giai đoạn 1: kiến trúc lai, R2 + join sao, 10M dòng, có cập nhật đồng thời.** Nội dung: (a) bộ điều phối + EXPLAIN (§4.1, §4.5); (c) delta (§4.3); (b) `Scan`, cột khoá, `TopNPerGroup` (§4.2); join sao dimension broadcast + aggregate sau join (§5 mục 1, 4); phân lớp R (§4.4 mục 1, bỏ hạ làn cho R). Chia 1a (điều phối, delta, EXPLAIN, top-N/R2: khoảng 3 500 dòng Go + 3 000 dòng test) và 1b (join sao/aggregate trên lô: khoảng 3 000 + 2 500), MỘT cổng chung. Kích thước thô: 55-70 giờ agent [ƯL; ngân sách dự án 100 giờ xây (docs/27) nên đây là phần lớn, cần người dùng chấp nhận].
- *Nghiệm thu đúng:* với `scripts/workload-multi` ở 300k (nhanh) và 10M: q03 trang 1-3 bằng `postgres:16` từng dòng (có thứ tự); q01-q15 giống PG16 sau chuẩn hoá (dùng `cmd/pgdiff`); trường hợp rn hoà: tổng thứ tự có `number`.
- *Nghiệm thu dữ liệu đang đổi (yêu cầu docs/29 §5; điều phối gọi là M2, lưu ý `docs/12` M2 là huỷ câu lệnh, ta kiểm cả hai):* (i) ack-rồi-đọc 10 000 lần; (ii) tải `load/*.sql` (đặt giữ chỗ, đơn, nhập giá) song song với q03/q05/q13: q13 luôn 0 dòng ở mọi lần đọc `strong`; (iii) khoá chặn (barrier): dừng tải, so mọi truy vấn với PG16 trên cùng dữ liệu; (iv) kill -9 giữa tải rồi so; (v) bộ test thuộc tính bổ đề §4.3: dữ liệu ngẫu nhiên, UPDATE/DELETE ngẫu nhiên giữa hai lần dựng view, ở S < A và S > A, kết quả toán tử lai = quét core.
- *Nghiệm thu hợp đồng:* `strong`/`bounded`/`session`/`eventual` mỗi mode một test; view bị chặn (tạm dừng follower) vẫn trả đúng nhờ delta khi delta nhỏ, và lùi + in lý do khi delta lớn; không bao giờ trả cũ mà không đúng hợp đồng.
- *Nghiệm thu EXPLAIN:* chuỗi khuôn mẫu §4.5; test "kế hoạch Seq Scan + sort cho q03 là thất bại".
- *Đo ở G3 (qua `perf-run.sh`, D32):* q03 ở 10M/20M/50M, nóng, p50/p95, nền và trong lúc `load/*`; q05/q06/q07/q12; trễ view và kích thước delta trong lúc nhập; so PG16 và ClickHouse (R2: `scripts/r2-paginated-quota`). Mục tiêu docs/24 phần D (trần 1 s, lý tưởng < 0,5 s, p95, chờ xác nhận). Khởi điểm so sánh: `r2perf` 5,3 s ở 10M [đo]. KHÔNG ngoại suy từ 300k.
- *Yêu cầu khác có thể bị ảnh hưởng:* R1 (đọc điểm không đổi, kiểm không chậm hơn); lanes/R-prio (bỏ hạ làn cho R có thể cho R chiếm nhiều helper hơn: đo p99 đọc điểm); bộ nhớ (cột khoá thêm RAM: kiểm docs/24 phép đo 1 memmap); khôi phục (không đổi); tương thích P1 (không đổi cú pháp; `EXPLAIN` đổi chuỗi: kiểm bộ pgdiff/suite không so EXPLAIN); G2 (kill -9).
- *Luật dừng (D35):* tối đa hai vòng tối ưu cho mỗi mục đo (R2, join sao, delta); sau đó ghi giới hạn và đi tiếp.

**Giai đoạn 2: join tổng quát trên cột.** Hash join hai bảng lớn với build thực thi trước, bộ lọc runtime, INDEX-NL, semi/anti, ngoài; cột chuỗi phẳng cho văn bản nhiều giá trị; bảng băm khoá trên arena nếu chưa làm ở 1. ~4 000 dòng + 3 000 test, 35-45 giờ [ƯL]. Nghiệm thu: q05-q12 toàn bộ trên đường lai ở 10M/50M, bằng PG16; join thiếu bộ nhớ lùi/spill đúng. Đo: join fact-fact 10M×15M; hiệu quả bộ lọc runtime (so có/không). Ảnh hưởng: bộ nhớ truy vấn, làn.

**Giai đoạn 3: cách ly và độ mới dưới tải.** Quét không giữ khoá suốt lần (epoch), hồ người giúp cho R, sàn CPU cho áp view, đo tốc độ áp view và pacing. ~2 000 dòng + test, 20-30 giờ [ƯL]. Nghiệm thu: p99 đọc điểm và R2 trong lúc nhập 1M dòng và quét nặng; lag view ≤ ngưỡng. Quyết định theo số đo của giai đoạn 1: chỉ làm nếu thấy đảo ưu tiên/khoá.

**Giai đoạn 4: bố cục vật lý (CHỈ nếu đo chỉ ra).** Sort key và zone map bỏ chunk, co-partition theo khoá join, projection nối sẵn, từ điển dùng chung, thống kê. Có đổi catalog (cộng thêm). Chỉ mở nếu giai đoạn 1-3 không đạt G3 ở một mục đo cụ thể, và đo cho thấy bố cục là nguyên nhân (D35: không tối ưu theo suy đoán).

## 7. Workload kiểm chứng (`scripts/workload-multi/`)

Lược đồ (`schema.sql`): `carriers`(3), `stores`(40), `customers`(n/20), `sims`(n; khoá chuỗi 11 ký tự `09` + 9 chữ số; carrier_id, store_id, category(5), price, store_rate, status, updated_at), `stock`(store×category, qty, reserved), `orders`(n/10), `order_items`(0,15·n), `history`(3·n). Bộ sinh dữ liệu tất định (`gen.sql`, tham số `n` và `seed`, băm `hashtext(k||':'||salt)`; `run-pg.sh dump DIR` xuất CSV để nạp STASK cùng byte). Tỉ lệ nhà mạng 30/50/20, trạng thái 70/10/20, 5 danh mục.

Mười lăm truy vấn (`queries/`): q01 tra khoá + join 2 dimension (R1); q02 trang theo khoá (R1); q03 R2 quota/trang 1-3 (`psql -v page=`); q04 facet nhiều nhóm; q05 join 5 bảng + aggregate; q06 join + top-N khách; q07 join dimension trên aggregate; q08 top-3 theo cửa hàng (cửa sổ có lọc); q09 history ⨝ sims ⨝ carriers, HAVING, top-N; q10 anti-join (NOT EXISTS) + group; q11 danh sách có tổng (`count(*) OVER ()`, 2 join, OFFSET); q12 chuỗi thời gian (LEFT JOIN + `date_trunc`); q13 BẤT BIẾN tồn kho (phải 0 dòng ở mọi lần đọc `strong`); q14 trung bình trượt 7 ngày (cửa sổ trên aggregate); q15 tìm trang (tiền tố + giá + nhà mạng + tổng).

Tải đồng thời (`load/`, qua `pgbench` trong container PG; STASK dùng cùng kịch bản qua driver pgjdbc/psql): `reserve.sql` (trọng số 5: một CTE đổi `available→reserved` và tăng `stock.reserved`, một giao dịch), `release.sql` (2), `order.sql` (2: chèn đơn, dòng đơn, lịch sử), `import_prices.sql` (một phiên cứ vài giây, 1% sim đổi giá nhiều dòng kèm lịch sử, mô phỏng nhập Excel). Bất biến tồn kho `stock.reserved = số sim 'reserved'` giữ qua mọi giao dịch (q13).

**Đã kiểm trên postgres:16 (Docker `postgres:16`, 300 000 sims; container đã xoá):** nạp xong (stock 200, orders 30 000, order_items 45 000, history 900 000); cả 15 truy vấn chạy; q03 trả 100 dòng mỗi trang, tỉ lệ 30/50/20 và giá trong 1-3M ở cả ba trang; q01 trả đúng 1 dòng, q02 50 dòng, q04 45, q05 12, q06 20, q07 20, q08 120, q09 50, q10 15, q11 50, q12 29, q13 0 dòng, q14 90, q15 20; mỗi truy vấn 150-530 ms tính cả chi phí `docker exec` (không phải số đo hiệu năng). Tải 10 giây (8 máy khách) chạy xong, 0 giao dịch lỗi, q13 vẫn 0 dòng sau tải. Chưa chạy trên STASK; chưa kiểm số liệu ở 10M (sẽ dùng `n` = 10 000 000 trên `postgres:16` làm oracle ở giai đoạn 1; lưu ý đĩa).

## 8. So với `r2perf` (điều kiện 3)

`r2perf` song song hoá bộ thực thi dòng tổng quát: lọc ở bước quét, sắp xếp song song, bỏ qua `row_number` nhóm đồng hạng; 23,1 → 5,3 s ở 10M [đo], vẫn vật hoá 1M dòng và sắp xếp chúng, không dùng cột. Giai đoạn 1 ở đây đổi kế hoạch VẬT LÝ: quét cột có lọc → heap top-N theo nhóm với cắt tỉa theo khoá dẫn đầu → chỉ tính md5 cho ứng viên → vá delta → lấy 100 dòng từ core. Ước lượng chi phí: quét lọc 10M dòng × 4 cột trên cột nén và vector chọn, md5 cho ~10^5 ứng viên; đây là ƯL để đo, KHÔNG phải kết luận, và không so được với số ClickHouse đã có (0,9 s, một lần đo, VM chung). Đường `r2perf` giữ làm đường lùi R4.
Ý kiến khác biệt với một điểm trong yêu cầu: tôi không coi chỉ mục `(category, price)` là đường chính cho R2 vì bộ lọc (category, price) khớp khoảng 10% số dòng của bảng 10M (1/5 danh mục × ~1/2 khoảng giá, tính từ bộ sinh của `r2perf`; PG16 cũng dùng bitmap scan 1,7 s trong 6,4 s ở 10M [đo, r2perf]); coordinator vẫn có đường INDEX (luật R3) và G3 sẽ đo; kết quả đo quyết định.

## 9. Mốc kiểm soát trong quy trình

Trước khi đi tiếp bất kỳ giai đoạn nào: kiểm lại các dòng [VL, chưa đọc lại] mà giai đoạn đó dựa vào; ghi tiền lệ vào dòng docs/14 của nhiệm vụ.

## 10. Điều chưa biết (phải đo, không đoán)

1. Tốc độ áp view khi nhập 1M+ dòng (docs/15 §2 W8 mục 1: chưa chứng minh) và hệ quả lên kích thước delta.
2. Chi phí quét log lấy khoá cho delta 10^4, 10^5, 10^6 khoá; chi phí `GetAt` hàng loạt.
3. Bộ nhớ view với khoá chuỗi 50M (map Go hiện tại và phương án arena).
4. Độ chọn lọc thực của R2 trên dữ liệu thật; có cần chỉ mục `(category, price)`.
5. Giá của đảo ưu tiên khoá đọc (§2) dưới tải thật.
6. Giá trị `HybridDeltaMax`, `HybridWaitMax`, `BroadcastMax` (đang là giá trị khởi điểm, không phải kết quả đo).
7. Số phiên bản mỗi SIM trong 60 ngày và tải đỉnh (chờ người dùng, docs/27 §6).
8. Hành vi view khi `ALTER TABLE` thêm cột (cột không có trong view đến lần khởi động, `columnar.go alterLocked`): coordinator lùi đúng nhưng chưa có test lai.

## 11. Câu hỏi chỉ người dùng trả lời được

1. Xác nhận ngưỡng SLO-R2 (trần 1 s, lý tưởng < 0,5 s, p95, nóng, 30-50M) cho cổng G3.
2. Chấp nhận quy mô giai đoạn 1 (55-70 giờ trong ngân sách 100 giờ xây) hay thu hẹp (ví dụ 1a trước, 1b sau).
3. Quy tắc khi một nhà mạng có ít dòng hơn hạn ngạch trang (đã mở ở `scripts/r2-paginated-quota`): trả thiếu hay bù từ nhà mạng khác.
4. Lược đồ thật tối thiểu của `sims`/`orders`/`stock` (tên cột, kiểu, số cột) để thay bộ sinh tổng hợp.
5. Phần cứng và đĩa cho phép đo 50M/100M (đĩa ~50 GB đã hứa cho bậc 50M; 100M cần nêu rõ).

## 9. Cắt lại giai đoạn thành lát dọc (người dùng, 2026-10-31; thay cách chia 1a/1b ở §5)

Giai đoạn 1 phải chứng minh MỘT CHUỖI hoàn chỉnh, mỏng ở từng mắt xích, thay vì đi sâu từng mắt xích một:

```
giao dịch cập nhật nhiều bảng (đơn hàng + tồn kho + giá)
  -> log bền
  -> delta + chiếu cột
  -> truy vấn R2 có join, cùng một snapshot
  -> R được ưu tiên khi đang nhập (import)
```

Chuỗi này đạt ở 10 triệu dòng rồi 30 triệu dòng thì ý tưởng cốt lõi được chứng minh; khi đó các tính năng tương thích còn lại chỉ là phụ trợ. Lý do cắt dọc: cách chia cũ (1a điều phối + delta + R2, 1b join, giai đoạn 3 cách ly) dễ biến thành tối ưu từng mắt xích trong khi chuỗi chưa chạy hết, đúng rủi ro "chăm chăm một tiêu chí".

Lát 1 (mỏng, đủ cả chuỗi): tối thiểu của mỗi mắt xích. (1) Một giao dịch ghi `orders`, `order_items`, `stock` cùng lúc. (2) Delta: khoá đổi trong (khoảng giữa view và snapshot) đọc từ lõi, phần còn lại đọc từ cột (§4 lemma delta). (3) Truy vấn R2 có join một bảng chiều nhỏ (nhà mạng hoặc kho) trong cùng snapshot, `EXPLAIN` nêu rõ lượng đọc từ lõi, delta, cột. (4) Một tải nhập (cập nhật 1% SIM trong một giao dịch) chạy đồng thời; R giữ độ trễ, T không bị chậm, và truy vấn tồn kho q13 luôn đúng. (5) `kill -9` giữa tải và kiểm đúng sau phục hồi. Điều kiện đạt: kết quả giống PostgreSQL 16 từng dòng, không đọc cũ mà không biết, và số đo (sạch, ở G3) so với SLO-R2.
Lát 2: làm sâu từng mắt xích theo số đo (join cột tổng quát, bộ lọc runtime, cách ly tài nguyên dưới tải, bố cục vật lý). Quy tắc D35 (hai vòng mỗi hạng mục) vẫn áp dụng.

