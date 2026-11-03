# 面向 256 MB 级设备的在线 Writer 配置

在 256 MB 级设备上使用磁盘文本索引时，建议从下表开始。可以直接复制 [README 中的 Go 配置](../README.md#writer-memory-tuning)。这些数值来自桌面文本负载实验，未改变库的默认值。

| 控制项 | 推荐起点 | 选择依据 |
| --- | --- | --- |
| `WithNumAnalysisWorkers` / `WithMaxConcurrentBatches` | 2 个分析 worker / 2 个在途批次 | 允许写入重叠；并发生产者用 `BatchFunc` 在取得名额后再分配文档。 |
| `WithMaxBatchInputBytes` | 512 KiB | 给单批输入设上限；字段名和更新、删除选择器也计入。 |
| `TextChunkOptions`（仅流式文本导入） | `MaxChunkBytes: 64 << 10`、`BatchSize: 4`、`OverlapBytes: 0` | 每批最多 256 KiB 文本，为字段和来源信息留出余量。向量分块仍由应用决定。 |
| `WithMergeBufferSize` | 64 KiB | 控制合并输出缓冲；合并时仍有其他内存开销。 |
| `WithMergePlanningByBytes(true)` 下的 `FloorSegmentSize` / `MaxSegmentSize` | 256 KiB / 4 MiB | 按编码字节规划，避免因文档数少而反复合并大文本段。 |
| `SegmentsPerMergeTask` | 4 | 限制一次合并的输入段数；其余合并参数沿用 `mergeplan.DefaultMergePlanOptions`。 |

若主要写小文档且设备仍有内存余量，先只把在途批次从 2 调到 4：在 8 个生产者的实验中，写入含关闭从 1.77 秒降到 1.63 秒，峰值从 49.3 MiB 升到 64.3 MiB。查询优先时先在目标设备记录常用查询的 p95 和段数，再考虑调整 4 MiB 的合并目标。

**先区分三类开销：**

- **输入与分析。** 文本、字段名和选择器的字节数适合做廉价的批次保护。同样大小的文本会产生不同数量的词项和位置，无需给它们套一个不可靠的固定膨胀倍数。对于大文件，使用 `ImportText` 流式读取；本实验每块最多 64 KiB、每批最多 4 块。向量的分块、生成和批次大小仍由应用决定，文本输入限额不计 embedding 或原生索引内存。
- **等待构造的批次。** 对并发生产者使用 [`BatchFunc`](writer-write-controls.md#admission-and-rejection)，在获得名额后再读数据、分配文档。队列里只保存轻量描述符。普通 `Batch` 收到的对象已经构造完成；如果回调提前捕获了大切片，延迟构造也无法节省这部分内存。
- **持久化与合并。** 默认规划器按文档数评估大小，4 个大文本块只算 4 份文档。`WithMergePlanningByBytes(true)` 改为按编码字节分层，按未删除文档的比例估算有效字节。过大的待持久化 segment 组先直接落盘，之后由磁盘规划器挑选合并组合。前台和后台仍可并行；不需要为每次分配计费或串行化全部写入。

`FloorSegmentSize` 和 `MaxSegmentSize` 在字节模式下都以字节为单位。现有规划器只把有效大小小于 `MaxSegmentSize/2` 的 segment 列为候选；组合的有效大小须小于 `MaxSegmentSize`。实际输出可能与估算不同。大批次不会自动拆分，原本就很大的 segment 也不会因开启选项而拆分。Reader 会固定旧快照，需要时才持有，使用完及时关闭。

复用同一份 `Config` 打开多个 Writer 时，每个 Writer 现在都有独立分析队列；设备上的总 worker 数仍是各 Writer 配置之和。

## 实验结果

以下是每组 3 个独立进程的中位数。峰值是 Windows 报告的进程历史峰值工作集，写入耗时包含 `Writer.Close()` 收尾；查询列是每轮 100 次热查询的 p95 再取中位数。[27 次完整记录](benchmarks/writer-memory-20261103/results.json)还包含采样堆峰值、累计分配、GC 次数、合并次数和落盘大小。

| 磁盘文本负载 | 进程峰值 MiB（原 → 新） | 写入含关闭，秒（原 → 新） | 查询 p95，ms（原 → 新） |
| --- | ---: | ---: | ---: |
| 64 MiB 密集短词，再替换为 16 MiB，保留旧 Reader | 279.8 → **70.4** | 12.64 → **7.90** | 0.0310 → 0.1046 |
| 32 MiB 混合词，再替换为 8 MiB，保留旧 Reader | 144.7 → **50.6** | 4.31 → **3.49** | 0.0105 → 0.0251 |
| 8 个生产者，写入 32 MiB 小文档 | 99.8 → **49.3** | 1.45 → **1.77** | 0.0285 → 0.0493 |

前两行原配置使用 `f21fa03`，分析 worker、批次名额、批次输入上限、合并输出缓冲和合并扇入均与新配置相同。不同之处是原实现按文档数规划，新实现按字节规划，并限制了 segment 构建的推测性预分配。最后一行比较原默认配置与新推荐配置，包含限制并发和延迟构造的综合影响。

密集文本三轮的峰值范围为 **278.1–302.7 → 68.6–80.1 MiB**。同一场景的累计分配中位数从 18.35 GiB 降到 6.65 GiB，最终目录文件数（含快照）从 2 增至 15，磁盘大小从 12.3 增至 12.7 MiB。这解释了主要取舍：减少大合并的反复重写，写入和内存受益，查询要访问更多 segment。查询绝对时间随主机和负载变化，这个实验只测一种查询。

## 怎么调

- **突发请求多：** 在相同新配置下，64 个生产者先构造再调用 `Batch` 的峰值中位数为 83.0 MiB、写入耗时 1.71 秒；改用 `BatchFunc` 为 50.2 MiB、1.75 秒。应先移动构造时机，再考虑限制排队数量。
- **查询优先：** 在目标数据上同时测 segment 数和常用查询。单纯把最大值从 4 MiB 放宽到 8 MiB，在本次密集文本上最终仍有 15 个文件，[三轮补充实验](benchmarks/writer-memory-20261103/merge-8mib.json)未显示稳定的查询改善。分层下限和预算同样影响最终布局。

## 范围与复现

2026-11-03，Windows/amd64，Intel i5-13600KF，Go 1.24.6，`GOMAXPROCS=2`，未设置专用 GC 策略或进程内存限制。所有运行串行，使用独立进程和临时磁盘索引。流式用例生成输入、保留旧快照进行替换、验证旧快照计数、关闭写入器、重开索引并验证文档数和搜索结果。并发用例为 8192 份 4 KiB 文档，每 64 份一个批次，启用存储和词位置。测量覆盖真实的持久化、合并、重开和查询，不含网络、embedding、向量库或其他应用进程。

这些结果支持将该配置用于真机试跑。桌面进程工作集不能直接换算成 256 MB 设备的可用内存；目标设备需要为操作系统、其他服务和实际向量后端留余量。特别是长期不关闭的 Reader、不同压缩率和更大的索引，都会改变峰值。

当前版本可直接运行一个用例：

```powershell
$env:GOMAXPROCS = '2'
go test . -run '^TestWriterMemoryExperiment$' -count=1 -v -args -writer-memory-case stream -writer-memory-profile edge -writer-memory-mib 64 -writer-memory-dense
```

在 Windows 或 Linux 上会读取 OS 的进程历史峰值；其他平台该指标为 0，可另接系统监视器。完整比较可用 [PowerShell 脚本](run-writer-memory-experiment.ps1)。若需重建原版本，在 `f21fa03` 的独立 checkout 中复制本版本的 `writer_memory_*_test.go` 四个实验文件，再分别编译两个版本的根包测试程序：

```powershell
# 在原 checkout 中执行（已复制实验文件）
go test -c -o D:\bench\baseline.exe .
# 在当前 checkout 中执行
go test -c -o D:\bench\candidate.exe .
.\docs\run-writer-memory-experiment.ps1 -BaselineBinary D:\bench\baseline.exe -CandidateBinary D:\bench\candidate.exe -OutputPath D:\bench\results.json
```

不传 `-BaselineBinary` 可以只跑当前版本。单独复现 8 MiB 对照时，在密集文本命令后追加 `-writer-memory-merge-mib 8`。
