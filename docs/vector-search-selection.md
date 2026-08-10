# Bluge 向量搜索技术选型

日期：2026-08-10

## 结论

没有脱离约束的“当前最优”方案。对这个仓库，当前更重要的约束是：复用
成熟 ANN 实现、主流 64 位桌面/服务器平台可构建、Go 默认构建保持
`CGO_ENABLED=0`，并且不把向量算法复制到 Bluge 中。

本分支的实际选择是 `USearchVectorBackend`：USearch 2.26 负责 HNSW 图、距离
计算、删除、压缩和序列化；我们只维护 Bluge 文档 ID 到 `uint64` key 的映射，
以及每个 vector field 的 manifest。Rust adapter 位于仓库根部的独立
`usearch-ffi` 子项目，编译为独立动态库，Go 通过 `purego` 调用窄 C ABI。
GitHub Actions 在 Windows、Linux 和 macOS 的 amd64/arm64 原生 runner 上验证
插入、ANN 查询、Bluge Query 过滤、更新、删除、重开和 `CGO_ENABLED=0`。

FAISS 仍然是生态和索引类型最强的候选，但在本次 Windows 约束下，它需要先
解决 FAISS C API DLL、BLAS/编译器和分发矩阵；因此保留为下一条对照 backend，
而不是在没有可运行 DLL 的情况下把它写进默认路径。

## FAISS 实际核对

本次没有只根据印象排除 FAISS，而是核对并尝试了它的 Go 生态：

- `github.com/blevesearch/go-faiss` v1.0.26 的源码直接使用 `import "C"` 和
  `#cgo LDFLAGS: -lfaiss_c`，所以它本身不能满足 `CGO_ENABLED=0`。
- 它依赖先构建带 `FAISS_ENABLE_C_API=ON` 的 FAISS；该项目的 C API 本身
  存在，因此理论上可以像本分支一样再包一层稳定 C ABI + `purego`。
- 在当前 Windows 机器上用 Bleve 的 FAISS fork、CMake、Clang 和 MinGW
  generator 配置 CPU/C API 构建时，配置在 `FindBLAS` 阶段停止：本机没有
  可用 BLAS 库。FAISS 的实际 DLL、BLAS 和工具链分发闭环尚未形成。

所以 FAISS 仍是长期生态优先级更高的对照，但当前实现选择了已经形成六平台
原生库构建和 Go FFI 验证闭环的 USearch；这是一项可验证的工程
选择，不是声称 USearch 的总体生态超过 FAISS。

`FlatVectorBackend` 保留为精确 recall@k 和分数语义基线。它不是大规模数据的
最终性能方案，也不是 ANN 算法的替代实现。

## 候选比较

| 方案 | 成熟度/生态 | cgo-free | 适合当前仓库的程度 | 主要问题 |
| --- | --- | --- | --- | --- |
| FAISS + `go-faiss` | 很高，索引类型最全 | `go-faiss` 默认依赖 cgo；独立 C API DLL 可做到 Go cgo-free | 下一阶段 native 对照 | C++ ABI、BLAS、Windows 构建和分发矩阵复杂 |
| USearch 2.26 + Rust C ABI | 主流度低于 FAISS，但跨语言生态和单文件 HNSW 清晰 | 是，Go 只用 `purego` 加载动态库 | 当前首选 | Rust crate 内部通过 `cxx` 调用 USearch C++ 核心，native library 需要单独分发 |
| Rust `hnsw_rs` | Rust ANN 生态中较成熟 | FFI 直接嵌入否；WASM 可行 | 后续纯 Rust 对照 | 需要自定义稳定 ABI、持久化和更新语义 |
| Rust `instant-distance` + WASM | 纯 Rust、API 小 | 是，使用 wazero | 可作为无 native runtime 的后续实验 | ANN 功能、更新语义和运维生态弱于 USearch/FAISS |
| ZVEC C API + purego | 本机已有 Windows DLL 和验证过的 wrapper | 是，Go 可 cgo-free | FFI 参考和备选 | 它是完整向量数据库引擎，不是窄 ANN 库；会重复 collection/schema/persistence |
| C/C++/Rust C ABI + Go | native 库选择多 | 可以；ABI 与 Go 的动态加载是两个问题 | 适合作为可选 backend | 需要薄平台加载层、生命周期、内存和动态库分发契约 |
| 精确 flat | 算法简单、结果可验证 | 是 | 当前已实现 | `O(N * D)` 查询，不适合百万级以上高并发 ANN |

这里的 `cgo-free` 指 `CGO_ENABLED=0` 下 Go 仍可构建和运行；它不等于底层
算法必须用 Go 重写，也不等于不能使用 C/C++/Rust。稳定 C ABI 和
`purego.RegisterLibFunc` 共同解决了“独立编译 native library + Go 调用”的
闭环。平台差异只剩打开/关闭动态库：Windows 委托 `LoadLibrary`，Linux/macOS
委托 `purego.Dlopen`；这几行 shim 与 zvec 的 pure-Go binding 一致。

## USearch 的性能边界

USearch 的性能优势不是“所有场景都比 FAISS 快”，而是针对单机 CPU HNSW
路径做了很窄的取舍：

- 图结构、label/key、向量存储和搜索循环都保持紧凑，减少通用索引层的间接访问；
- 距离计算交给 NumKong；同一个 native library 内包含多个 ISA kernel，运行时
  用 CPU capability 选择路径，并保留 `serial` fallback，不按 baseline/SSE/AVX
  拆分发布包；
- `f32` 以外还可以走 `bf16`、`f16`、`i8` 等量化路径，但本 adapter 当前固定
  使用 `f32`，没有把量化误算成免费收益；
- 只比较 HNSW CPU 查询时，不需要 FAISS 的 BLAS、IVF/PQ 训练和 GPU runtime
  依赖，因此小数据集、单机嵌入式场景的启动和部署成本更低。

FAISS 的硬件适配范围更宽：Flat/IVF/PQ/HNSW/NSG、成熟的 CPU 优化、GPU
索引和更完整的训练/量化工具链都属于它的优势。USearch 当前这条路径没有
FAISS GPU 那种 CUDA/HIP 后端；它的“硬件适配”主要是 CPU SIMD dispatch，
不是跨硬件平台的完整加速层。因此在 GPU、IVF/PQ、海量索引训练或需要大量
索引类型时，FAISS 仍应是优先对照。

官方 benchmark 只能说明给定数据集、维度、`K`、召回率、线程数、HNSW
`connectivity`/`expansion_search` 和 CPU 下的结果。严谨比较必须固定这些
变量，并同时记录 recall@K、build/update/delete、内存、sidecar 大小、冷启动
和 tail latency。当前 DLL 的 ABI probe 会报告 `Compiled` 与 `Available`
ISA；在本机 Clang 构建中实际是 `serial, haswell, skylake` 编译、`serial,
haswell` 可用。若只报告 `serial`，workflow 会拒绝发布该 artifact。

## 当前实践

向量字段通过 `NewVectorField` 加到 Bluge 文档中。对于 flat/USearch 旁车
backend，它不会进入 zapx 的文本倒排和 stored-field 数据；对于
`EmbeddedUSearchVectorBackend`，字段名会进入 segment 的字段目录，真正的
向量索引以 opaque native payload 写入 vector section：

```go
config := bluge.DefaultConfig(indexPath).
    WithVectorBackend(bluge.NewFlatVectorBackend(indexPath + ".vectors"))

writer, err := bluge.OpenWriter(config)
if err != nil {
    return err
}

doc := bluge.NewDocument("doc-1").
    AddField(bluge.NewTextField("body", "vector search")).
    AddField(bluge.NewVectorFieldWithSimilarity(
        "embedding", []float32{0.2, 0.8}, bluge.VectorCosine))
if err := writer.Insert(doc); err != nil {
    return err
}

reader, err := writer.Reader()
if err != nil {
    return err
}
hits, err := reader.VectorSearch(ctx, "embedding", []float32{0.1, 0.9}, 10,
    bluge.NewTermQuery("vector").SetField("body"))
```

常用的请求式 API 和混合搜索在 Go 层提供：

```go
vector := bluge.NewVectorSearchRequest("embedding", query).
    SetK(20).SetCandidates(200)
hybrid := bluge.NewHybridSearchRequest(textQuery, vector).
    SetK(20).
    SetFilter(filter).
    SetFusion(bluge.HybridFusionRRF)
hits, err := reader.HybridSearch(ctx, hybrid)
```

混合搜索只把候选集合和分数带回 Go，再由 Go 做 weighted 或 RRF 融合；
native backend 不需要理解 Bluge Query。`TextCandidates` 和
`VectorCandidates` 用于控制召回/延迟折中，默认是 `max(100, 10*K)`。

flat sidecar 的当前格式是 `BLUGEVEC` v1，保存 field spec、document ID 和
`float32` 数据。USearch sidecar 是 `manifest.json` 加每个 field 的 USearch
二进制索引；manifest 保存 field spec 和 ID/key 映射。两者都把 L2 分数转成
负的平方距离，并把 dot/cosine 转成 Bluge 的“分数越高越好”约定。

Embedded USearch 不使用这些旁车文件：每个 segment 的 vector section 保存
backend 名称、维度、similarity、segment-local docID 映射和 USearch
`save_to_buffer` 字节。merge 时按 zapx 提供的 old-doc 到 new-doc 映射读取
旧 payload 中的向量并重建新 payload，因此写入阶段是 segment 追加，重建成本
集中到 merge。

过滤仍由 Bluge 先执行，因此 native backend 不需要复制 Bluge Query 语义。
sidecar backend 把允许的 `_id` 映射成 USearch key；Embedded USearch 直接把
命中的全局文档号二分映射成 segment-local key，并同时排除删除位图。key 数组
一次性传入 Rust，由 Rust `HashSet` 驱动 USearch filtered search；native 不会
回调 Go，Go 也不再为过滤查询取回整个 field 的距离结果。

## 已知边界

- 文本 snapshot 和 sidecar backend 是两个持久化对象。当前批处理先提交文本，
  再提交向量；进程若正好在两步之间崩溃，恢复时可能出现短暂不一致。嵌入式
  backend 则随 segment transaction 发布，解决了文件级原子性，但仍要依赖
  merge 重建和 native ABI 版本兼容。
- flat backend 每次写批会复制当前向量 map 并重写 sidecar，适合 MVP 和中小
  规模数据，不适合高写入吞吐。
- 向量 field 的维度和 similarity 在首次写入后固定；变更会在文本 batch
  之前返回错误。
- USearch native library 与 Go 程序是两个 artifact，版本升级必须重新构建并
  验证 C ABI；默认 Go 包不会自动下载动态库。显式 `WithLibraryPath` 会覆盖环境和
  默认候选路径，未指定路径时才尝试 `BLUGE_USEARCH_LIBRARY_PATH`、可执行文件
  旁和操作系统动态库搜索路径。部署契约是放在主程序旁，不是当前 work dir。
- 已有嵌入式 index 在读取时找不到 native library 会降级为 text-only reader；
  这不会静默接受新的 vector 写入，写入仍要求 native artifact。
- filtered search 属于 ANN 图内过滤；过滤极为稀疏时，召回率和延迟仍受
  `ExpansionSearch` 影响，需要按知识库数据分布测试并调参。文本过滤查询及
  允许 key 集合本身也仍有成本。
- zapx 原先预留的 `SectionFaissVectorIndex` 槽位现命名为通用的
  `SectionVectorIndex`，并保留旧名称作为兼容别名。payload 带 backend 标识，
  当前由 Embedded USearch 使用，不把 segment 格式绑定到 FAISS 或 USearch。

## 下一阶段门槛

先用 flat backend 建立固定数据集，记录：

- recall@1/10/100、分数和 tie-break 结果；
- 写入、更新、删除、重开后的 ID 一致性；
- 查询延迟、内存、sidecar 大小和批写放大。

下一步用同一数据集比较 flat 与 USearch 的 recall@k、查询延迟、构建时间、
更新/删除后的召回、冷启动、sidecar 大小和进程内存；再决定是否投入 FAISS
Windows C API DLL 或 Rust-WASM 对照。USearch 目前是 opt-in，不改变默认
text-only 构建。

native library 统一由 `Vector Engine Native Libraries` GitHub Actions workflow
验证和打包。每个 OS/64 位架构只有一个产物：Windows 固定 LLVM 20.1.8 和
MSVC ABI，Linux 固定 GCC 13，macOS 固定 Xcode 16.4，Rust 固定 1.90.0；不发布
Clang/GCC/Zig 或 baseline/SSE/AVX 的平行变体。

推送相关改动会自动触发，也可以手动触发：

```console
gh workflow run usearch-ffi.yml --ref vector/native-ffi
gh run list --workflow usearch-ffi.yml
```
