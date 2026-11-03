# Minimal online write controls

These options bound work admitted by the top-level `bluge.Writer`. They do not
provide a heap/RSS ceiling and do not change `MemoryUsed()`. All are disabled by
default. They do not apply to `OfflineWriter`, direct `index.Writer` calls, or
queries, and do not add controls to vector backends.

```go
config := bluge.DefaultConfig("index").
    WithMaxConcurrentBatches(1).
    WithMaxDocumentInputBytes(256 * 1024).
    WithMaxBatchInputBytes(2 * 1024 * 1024).
    WithBatchStats(true)

writer, err := bluge.OpenWriter(config)
if err != nil {
    return err
}
defer writer.Close()
```

The example limits are application choices, not recommended memory budgets.
`WithMaxConcurrentBatches(0)` means unlimited; negative values fail at
`OpenWriter`. Both byte limits use `uint64`, with zero disabling each limit.
The byte limits and statistics work independently of the concurrency limit.

## Admission and rejection

A limited `Batch` acquires a slot before input validation and analysis, holding
it until the complete public call returns, including segment construction and
the existing synchronous persistence wait. `Insert`, `Update`, `Delete`,
`InsertMany`, and `UpdateMany` submit through `Batch`. Waiting for a slot holds
no internal index lock, so existing writes and persistence can continue.
`Close` immediately wakes slot waiters with `segment.ErrClosed`; the underlying
Writer retains its existing drain and persistence behavior. Slots are released
on both success and error. Admission does not guarantee FIFO ordering.

When several producers read or allocate their inputs concurrently, construct
documents **after** admission with `BatchFunc`:

```go
err := writer.BatchFunc(ctx, func(batch *index.Batch) error {
    doc, err := loadNextDocument(ctx) // application code: read only after admission
    if err != nil {
        return err
    }
    batch.Update(doc.ID(), doc)
    return nil
})
```

Here `index` is `github.com/fy0/bluge/index`. `WithMaxConcurrentBatches` bounds
the total number of admitted builders and writes together. Waiting callers may
cancel without running their callbacks. A builder error or cancellation before
submission discards its changes; once submitted, the batch uses ordinary write
semantics. Builders should fill the supplied batch and avoid recursive writes
to the same writer. Keep queued inputs as IDs, paths or other small descriptors:
capturing a prebuilt payload in the callback still retains it while waiting.

Input limits are checked before analysis and before any index changes. Equality
with a limit is allowed. An oversized document or batch rejects the whole
batch, including its deletes; no splitting or partial submission occurs.
The error is recognizable with `errors.As`:

```go
if err := writer.Batch(batch); err != nil {
    var limitErr *bluge.InputLimitError
    if errors.As(err, &limitErr) {
        // Scope is "document" or "batch"; Actual and Limit are bytes.
        log.Printf("%s input: %d bytes exceeds %d",
            limitErr.Scope, limitErr.Actual, limitErr.Limit)
    }
    return err
}
```

For a document error, `Actual` is the complete size of the first oversized
document encountered. For a batch error, it is the complete batch sum.
Existing failure semantics after a batch has passed preflight are unchanged.

## Input byte definition

* Each document contributes `sum(len(field.Name()) + len(field.Value()))`
  **before analysis**, using existing `segment.Document` / `segment.Field`
  interfaces. It includes `_id`, stored-only fields, unindexed fields, and each
  occurrence of a repeated field. Lengths count bytes, not Unicode characters.
* A batch adds every inserted/updated document's contribution, plus
  `len(selector.Field()) + len(selector.Term())` for **every** update/delete
  selector. An update therefore counts both its document's `_id` and its
  selector. A delete-only batch still has an input size.
* Logical occurrences are counted independently even when they share backing
  storage. Slice capacity, object overhead, analyzer objects, tokens, postings,
  locations, and analysis-generated/composite content are not counted. Numeric
  and other encoded field values contribute their exposed encoded bytes.
  Virtual fields configured on the index are not document input.

`Document.Size()` is not used. Custom documents must expose stable fields and
values before analysis for these controls to cover that input. Data hidden
behind custom fields, or generated only by `Analyze`, cannot be measured through
the existing interface and is excluded; no estimate or new protocol is used.
In particular, `VectorField.Value()` is nil, so only its field name is counted,
not its embedding. Callers must not mutate batches/documents during submission.

## Minimal observation and remaining memory

With `WithBatchStats(true)`, `writer.BatchStats()` returns a synchronized
snapshot containing just `InFlightBatches` and `InFlightInputBytes`.
The count includes admitted calls (including running `BatchFunc` builders)
until return, excluding slot waiters. Bytes
are added once input validation succeeds and removed on return. During input
validation the call contributes to the count but not yet to the byte sum.
Both gauges return to zero after all calls finish, including failures.
When statistics are off, both values remain zero. When byte limits and
statistics are all off, the controls add no field traversal, even with the
concurrency limit enabled.

These are workload gauges, not actual heap usage or reserved memory. A slot
limit does not limit callers' already-built batches or waiting goroutines.
Other unbounded sources include analysis expansion, segment-building and
encoding workspace, retained buffers, background persistence and merges,
segments/snapshots/readers still in use, in-memory index storage, vector/native
backend allocations, queries/caches, and runtime/GC overhead. Batch completion
does not imply all resulting allocations have been freed. There is no aggregate
byte budget, dynamic backpressure, automatic tuning, or process-wide GC change.
