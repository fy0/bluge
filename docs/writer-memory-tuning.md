# Writer memory tuning and integer encoding

The top-level `Config` now exposes `WithNumAnalysisWorkers`,
`WithMergeBufferSize`, and `WithMergePlanOptions`. See the
[configuration example](../README.md#writer-memory-tuning).

The default zapx-bluge adapter previously accepted `mergeBufferSize` but dropped
it before constructing the output writer. Both adapter merge entry points now
carry it through to the actual buffered writer. The regression test observes
buffer flush sizes and checks that the merged documents can be read back.

## Integer encoding changes

Previously, the integer coder retained every raw chunk in `final`, then
allocated a second full-size buffer in `Write` to assemble compressed chunks.
It now compresses each chunk when that chunk is completed and accumulates only
the selected raw/compressed representation. `Write` emits those bytes directly
on the ordinary path. Chunk flags, offsets, and the segment format are unchanged.

Custom file-processing callbacks still run once per non-empty chunk during
`Write`. They can change the encoded length, so that path still assembles a
separate output buffer. Callback input capacity is restricted to its chunk to
prevent an appending callback from overwriting later chunks.

On `Reset`, byte buffers larger than 1 MiB are released instead of being kept
for subsequent terms. Smaller buffers are reused. This trades allocations on
repeated unusually large terms for less retained memory; it does not cap the
size of a term, document, batch, or active encoding operation.

## Allocation benchmark

Measured on 2026-11-02 with Go 1.24.6, Windows/amd64, Intel Core i5-13600KF.
The baseline is `ecb07b2` with the same benchmark added. Each case was run three
times before and after the encoding change:

```console
go test ./internal/zapxtext -run '^$' -bench '^BenchmarkChunkedIntCoder$' -benchmem -count=3
```

Each iteration resets and reuses one coder, adds 16 chunks, closes it, and writes
to `io.Discard`. Input construction is outside the timed region. The table shows
the range of allocated bytes per iteration across the three runs; initial
buffer allocations are amortized over the benchmark iterations.

| Input per iteration | Before, B/op | After, B/op |
|---|---:|---:|
| 16 small chunks, 64 bytes total | 192 | 0 |
| 16 repetitive chunks, 8 MiB total | 8,506,174–8,600,443 | 1,678–1,686 |
| 16 deterministic random chunks, 512 KiB total | 1,189,014–1,189,312 | 262–304 |

These are allocation totals for the integer coder, not peak live heap or RSS.
They exclude document analysis, segment construction, output storage, and merge
overlap. They do not predict the peak memory of the reported 300 MB workload.

## Operational limits

Each online writer owns its analysis queue, including writers opened from the
same configuration. Reducing workers limits simultaneous analysis, but a batch's analyzed
documents can all remain live until segment construction. Reducing the merge
output buffer does not bound other merge workspace. Optional byte-based merge
planning applies the configured maximum to in-memory merge inputs as well.
Applications should therefore also
bound batch size and concurrent submissions, using their own document sizes
and device measurements.

The segment builder also caps its speculative initial output allocation at
1 MiB. Its estimate comes from a pooled builder's previous workload; adding a
fixed 100 extra documents to a one-document estimate could reserve about 25 MiB
after a 256 KiB document. The buffer can still grow to fit actual encoded data.
See the [whole-process experiments](writer-edge-memory.md) for memory and
performance measurements across writing, merging, and reopening.
