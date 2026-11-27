# zapx-bluge segment core

This package is derived from Bleve zapx v17.2.0.  The default build remains
pure Go and does not compile FAISS, while vector section identifiers, address
lookups, and the public backend extension boundary are retained.

The on-disk format is owned by Bluge and is identified by segment type
`zapx-bluge`, version `3`. It is not a Bleve `zap` v17 segment. Version 3 keeps
the exact field statistics and block impact tables (offset after frequency and
location offsets; zero means no impact table) while dropping unused Bleve
metadata (the 4-byte file-writer callback ID length from the 36-byte footer,
the synonym section slot, and doc-value placeholder varints before vector
payloads). High-cardinality terms store 64-posting blocks containing:

1. The delta-encoded inclusive final document number for the block.
2. The block's non-dominated `(frequency, raw norm)` impact pairs.

The pairs let built-in BM25 scorers calculate exact block upper bounds using
the same arithmetic as document scoring.  Terms below 128 postings omit the
table and use the ordinary iterator path.

Every field metadata record also stores these unsigned varints after the
section address pairs:

1. Number of documents containing at least one indexed term for the field.
2. Sum of all term frequencies for the field.

These values let Bluge construct exact BM25 collection statistics without
scanning the term dictionary during query setup.  Version 3 does not read v1 or
v2 segments.  Any future change to postings headers, field metadata, or the
footer layout must increment the `zapx-bluge` version.

Segment construction consumes Bluge's analyzed token frequencies through the
internal `blugeidx` representation. Native fields reuse their token maps;
custom `segment.Field` implementations use an iteration fallback.
