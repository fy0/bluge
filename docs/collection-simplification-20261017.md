# Collection 简化与验证（2026-10-17）

这次调整保留“共享文本索引 + 按名字读写向量空间”的职责。模型迁移、
流量切换和回滚由应用控制；库负责快照、原子发布和文件回收。

实现变化：

- `Reader("model-a")` / `OpenCollectionReader(config, "model-a")` 只加载并固定
  指定空间；省略名字保持原来的全部空间行为。
- 创建空间只需 `VectorSpaceSpec{ID: "model-a", Dims: 768}`。字段默认
  `embedding`，相似度默认 cosine，模型描述可选。
- `UpsertDocuments(documents, versions...)` 可指定每条文档的 embedding 输入版本。
  修改无关元数据时复用版本，向量继续有效；embedding 输入改变时必须换版本。
  省略版本仍为每次更新生成新版本。
- 所有已有向量都有效时，不再每次查询构造完整 ID / native key 过滤集合。
  过期向量和业务过滤仍正确取交集。
- 自动清理只处理刚写入的空间，只保留当前代和 Reader 固定的旧代。
  删除了固定保留两代的策略，以及单独构建、排序回收计划的中间结构。
- 清理检查 catalog、commit 身份和文件存在性；内容哈希及向量元数据由
  `VerifyCollection(ctx, config, names...)` 或实际加载检查。完整检查仍会占用
  coordinator，宜按需调用。
- 向量 Reader 的文本快照借用关系由 Reader 自己处理，Collection 统一调用
  `Close()`，不再直接修改另一个对象的关闭状态或手动拆开释放其内部资源。

后续收尾删除了未发布旧格式的读取回退和自动升级路径，所有 Collection 文件
统一使用当前格式版本，并要求带校验和的 catalog。旧实验索引需要重新创建。
native ABI 和向量算法不变。下面是移除兼容路径前完成的前后性能测量。

## 小规模前后对照

同一台 Windows amd64 机器，Go 1.24.6，`GOMAXPROCS=4`，同一 native DLL。
每个模型 10,000 条、128 维，初次单批写入，再执行两轮各 1% 更新；32 个
查询各计时三次。使用固定种子生成的合成数据，后台合并关闭。两个时间点各
执行一次完整基准，计时期间未同时运行全仓测试。结果不代表真实业务规模。

| 指标 | 单模型：修改前 → 后 | 双模型：修改前 → 后 |
| --- | ---: | ---: |
| 最终总文件体积 | 15.52 → 8.37 MiB | 29.81 → 15.52 MiB |
| 查询 p50（各模型） | 0.4246 → 0.0634 ms | 0.4367 / 0.3813 → 0.0613 / 0.0633 ms |
| 查询 p95（各模型） | 0.5899 → 0.1027 ms | 0.6857 / 0.4785 → 0.0952 / 0.1123 ms |
| 两轮更新总耗时 | 333.5 → 253.2 ms | 1127.7 → 503.1 ms |
| 释放旧 Reader 后的清理 | 43.9 → 5.4 ms | 196.1 → 18.4 ms |
| recall@10（各模型） | 100% → 100% | 100% / 99.6875% → 相同 |

逐条查询的召回率数组也完全相同。基准逐项检查了每个 native key 对应的
float32 向量和当前文档；所有模型均为 10,000 条有效 native 记录。另行验证
了原始报告的字节汇总、最终保留代数、每代 payload 校验和及零残留临时文件。

体积下降主要来自去掉无 Reader 使用的额外一代，查询改善主要来自移除不必要的
全量过滤。不同运行的图内部结构不保证字节相同，更新和清理时间也受文件系统
波动影响。这不是 embedded 与分离布局本身的对照。

测量汇总位于
[benchmarks/collection-simplification-20261017](benchmarks/collection-simplification-20261017/summary.json)。
完整原始报告保留在本地 `tmp/collection-simplify-before/collection-{1,2}/report.json`
和 `tmp/collection-simplify-after/collection-{1,2}/report.json`，未纳入版本控制。
其中包含配置、DLL 哈希、原始延迟和逐查询召回率。两次运行均基于未提交工作区，
报告里的 HEAD 相同，不表示两次源码相同。

复现修改后的测量（输出目录必须不存在）：

```powershell
$env:BLUGE_USEARCH_LIBRARY_PATH = 'D:\codes\2026\bluge\usearch-ffi\target\x86_64-pc-windows-msvc\release\vector_engine.dll'
$env:GOMAXPROCS = '4'
go test . -run '^TestCollectionStorageBenchmark$' -count=1 -timeout=5m -v `
  -collection-bench-out=tmp/collection-simplify-repeat `
  -collection-bench-cases=collection -collection-bench-docs=10000 `
  -collection-bench-dims=128 -collection-bench-batch=10000 `
  -collection-bench-rounds=2 -collection-bench-queries=32
```

## 验证和剩余成本

真实 USearch DLL 下的全仓 `go test ./... -count=1 -timeout=10m`，以及
`go test -race . -run 'TestCollection|TestPreparedVectorFilter' -count=1 -timeout=5m`
均通过。覆盖按空间隔离、显式输入版本、过滤、资源释放、跨进程旧代保护、
进程被杀后的恢复、损坏检测及不支持的格式拒绝。

向量更新仍会写出一份完整新 generation；首次加载还会校验内容并读取文档
版本。存在过期向量或业务过滤时，仍有过滤集合转换开销。这里未引入增量日志、
新的过滤缓存框架或额外模型管理层。
