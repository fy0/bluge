# 流式文本分块导入

`Writer.ImportText` 是显式启用的纯文本入口。普通 `Insert`、`Update`、
`InsertMany`、`UpdateMany` 和 `Batch` 的行为不变，不自动分块，也不拆分用户批次。
不需要向量后端、embedding 或新增依赖；不适用于 `OfflineWriter`。

```go
config := bluge.DefaultConfig("index").
    WithMaxConcurrentBatches(1).
    WithMaxDocumentInputBytes(65 * 1024).
    WithMaxBatchInputBytes(256 * 1024)
writer, err := bluge.OpenWriter(config)
if err != nil {
    return err
}
defer writer.Close()

file, err := os.Open("large.txt") // 例如 50 MiB 的合法 UTF-8 文本
if err != nil {
    return err
}
defer file.Close() // ImportText 不关闭调用方的 Reader

result, err := writer.ImportText("articles/42", "body", file, bluge.TextChunkOptions{
    MaxChunkBytes: 64 * 1024,
    OverlapBytes:  0,
    BatchSize:     2,
})
if err != nil {
    // [0, CommittedChunks) 是返回成功的块序号范围；字节覆盖范围为
    // [0, CommittedEndByte)。失败批次的尝试范围在 err 中，可能已经应用。
    log.Printf("source deleted=%t, chunks=[0,%d), bytes=[0,%d): %v",
        result.SourceDeleted, result.CommittedChunks, result.CommittedEndByte, err)
    return err
}
```

这些限额只是调用示例，应按来源 ID、字段名长度等预留元数据空间。

## 参数与切分

| 参数 | 含义与非法值 |
| --- | --- |
| `MaxChunkBytes` | 单块文本的 UTF-8 字节上限，包含重叠；必须至少为 4，并能用 `int` 容纳额外 4 字节预读。零值无效，调用方必须显式指定。 |
| `OverlapBytes` | 重叠字节数的上界，默认 0；必须满足 `0 <= overlap < max`。按完整字符向下取整；必要时进一步减小，为新字符和向前推进留空间。 |
| `BatchSize` | 每批最多文档数，默认 0 表示 1；负数无效。现有批次输入字节限制还可触发提前提交。 |

所有位置和长度均为原始 UTF-8 **字节**，不是字符、token 或分析后的偏移。
来源 ID、字段名不得为空；文本字段不能使用 `_id` 或下述四个保留字段。
非法参数在读取和修改索引前报错。非法 UTF-8 在遇到时报错，之前的批次可能已提交。

对达到上限的块，在候选文本后半部分优先选择空行（LF/CRLF）、换行、Unicode
空白边界；都没有时按完整 UTF-8 字符强制切分。末尾不足上限时直接输出剩余文本。
不裁剪空白，不丢失字节，块的结束位置严格前进，也不产生仅重复重叠内容的尾块。
因此超长单段和没有空白的连续文本也能导入。字符保持完整，但长单词仍可能被切断。

## 写入与来源生命周期

调用链沿用 `Writer.Batch` 的并发槽、输入校验、字段分析、segment 构建和持久化。
每块只保存本块文本，使用标准文本分析器并保存词位置；不创建保存完整原文的父文档。
`sourceID` 是逻辑来源标识，一个来源对应一个文本字段和一组块。再次导入同一来源
（包括换文本字段、减小文本长度或改变分块参数）会替换其全部旧块。

流程是：校验删除选择器及首块 → 单独提交来源删除批次 → 分批提交新块。
空输入仅删除来源，不插入空文档。首块读取、UTF-8 或输入大小校验失败不会删除旧块。
每块实际大小均复用现有校验：`sum(len(field.Name()) + len(field.Value()))`，
包含 `_id`、来源及所有存储元数据。删除批次也计入选择器的字段名和 term 字节。
文本块上限并非文档输入上限；元数据的十进制长度会随序号、位置增长。
单块或删除选择器超限时返回可用 `errors.As` 识别的 `*bluge.InputLimitError`，
不会偷偷减小块或放宽限额。普通 `Batch` 仍整批校验、整批提交。

显式删除所有分块：

```go
err := writer.DeleteText("articles/42")
```

`ImportText` 和 `DeleteText` 在同一 Writer 上串行执行（包括不同来源），以简单方式
防止流式替换、删除交错后留下旧块，也限制同时准备的导入缓冲区。
普通写入调用依然遵循现有并发规则；应用不得同时通过普通 API 修改保留分块文档。
`Update(Identifier(sourceID), doc)` 和 `Delete(Identifier(sourceID))` 只操作普通 `_id`，
**不级联**处理分块；使用 `ImportText` 替换，使用 `DeleteText` 删除。
普通父记录若由应用自行维护，需由应用另行更新或删除。

## ID、元数据与搜索

`TextChunkID(sourceID, index)` 的格式为
`_bluge_text_chunk:<sourceID 的无填充 URL base64>:<十进制序号>`。
完整来源编码与分隔符消除拼接歧义，同一来源和序号得到相同 ID。
应用须将 `_bluge_text_chunk:` ID 前缀及以下字段保留给此入口。

| 字段常量 | 值 |
| --- | --- |
| `TextChunkSourceField` (`_chunk_source`) | 原始来源 ID，keyword 索引并存储，可用于精确查询和来源删除 |
| `TextChunkIndexField` (`_chunk_index`) | 从 0 起的块序号，十进制字符串，仅存储 |
| `TextChunkStartField` (`_chunk_start`) | 原文起始字节位置，包含，仅存储 |
| `TextChunkEndField` (`_chunk_end`) | 原文结束字节位置，不包含，仅存储 |

```go
reader, err := writer.Reader()
if err != nil {
    return err
}
defer reader.Close()
hits, err := reader.Search(context.Background(), bluge.NewTopNSearch(10,
    bluge.NewMatchQuery("检索内容").SetField("body")))
if err != nil {
    return err
}
for {
    hit, err := hits.Next()
    if err != nil {
        return err
    }
    if hit == nil {
        break
    }
    err = hit.VisitStoredFields(func(name string, value []byte) bool {
        // body 是当前块；来源 ID、序号和原文字节范围用于追溯。
        fmt.Printf("%s: %s\n", name, value)
        return true
    })
    if err != nil {
        return err
    }
}
```

查询需指定传入的文本字段；没有自动构建 `_all`。结果以块为单位，不自动合并来源。
短语只能匹配同一块内的词；重叠能保留部分跨边界短语，但不保证所有跨块短语可匹配。
强制切开的单词也可能影响词项匹配。词位置相对当前块；元数据偏移相对原文。
BM25 等评分使用块长度和分块后的文档/词项统计；重叠重复词项，可能产生同来源多次命中，
排序与整篇索引不同。本功能不修改评分、结果去重或查询合并逻辑。

## 失败、可见性与恢复

多个批次之间没有事务：删除成功后，读者可看到来源暂时消失或只包含前缀块。
中途读取失败、非法 UTF-8、后续元数据增长导致超限、构建或持久化失败均可能部分成功。
内存中尚未提交的块丢弃；之前返回成功的批次保留。结果中的
`SourceDeleted` 表示来源删除批次已返回成功；`CommittedChunks` 和
`CommittedEndByte` 仅报告返回成功的插入批次，不重复计算重叠字节。

底层 Batch 在错误返回前可能已应用索引变更，例如随后持久化失败。错误会另外列出
失败批次尝试的块序号与字节范围，并标注可能已应用；失败的删除批次也可能已应用，
此时 `SourceDeleted == false` 不保证旧内容仍在。结果不作跨批回滚或崩溃恢复承诺，
持久性继承现有 Writer（包括 `UnsafeBatch` 设置）的语义。

恢复方式是从头再次 `ImportText`，或调用 `DeleteText` 清理该来源。
无需保存全部旧块 ID、总块数或持久化清单；来源字段能删除先前版本及部分导入的所有块。
这保证正常返回成功的替换不残留旧尾块，但不提供全来源原子替换。

## 内存范围与验证

导入器保留固定预读缓冲区（`MaxChunkBytes + 4`，受 bufio 最小 16 字节约束）、
当前有界批次，以及必要时为字节限额校验提前生成的一块。不使用 `ReadAll`，不收集整个
文件、全部分块、全部 ID 或全部分析结果；每批结束后放弃该批次对象，避免 Reset 留住
已分析文档。缓冲区和文档可能在等待现有 Batch 并发槽时占用内存。
调用方提供的 Reader 可有自己的缓冲；阻塞的 Read 不能被 Writer.Close 中断。

这不是进程内存硬上限。分析器的 token、位置和词频展开、segment 构建与编码工作区、
后台持久化和合并、仍被 reader/snapshot 持有的 segment、内存目录、查询缓存、
向量后端的已有状态和 Go runtime/GC 都可能额外占用内存，Batch 完成不保证立即释放。
本实现不调整 GC，不扩展 OfflineWriter 或向量后端。

关键测试包含默认普通写入、纯文本搜索、精确大小边界、中文/emoji、长段落、重叠推进、
输入字节和并发槽兼容、来源替换/删除/重开、短语边界，以及读取和构建中途失败的范围。
50 MiB 生成流测试每次构建 segment 时检查读取进度与批次文档数，验证未先读完或积累
全部文本才开始分析提交。

```sh
go test . -run 'TestTextChunk|TestImportText' -count=1
```
