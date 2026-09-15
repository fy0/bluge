#ifndef BLUGE_USEARCH_H
#define BLUGE_USEARCH_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* Increment when the exported signatures or ownership rules change.
 * Version 3 is the baseline contract. Version 4 adds
 * bluge_usearch_index_reserve_threads; Go treats it as optional and keeps
 * ABI 3 libraries working with their default context pool. */
uint32_t bluge_usearch_abi_version(void);

/* Returned strings are owned by the DLL and remain valid until unload. */
const char *bluge_usearch_hardware_acceleration_compiled(void);
const char *bluge_usearch_hardware_acceleration_available(void);

/* Metric values shared with the Go adapter. */
enum {
    BLUGE_USEARCH_METRIC_L2 = 1,
    BLUGE_USEARCH_METRIC_DOT = 2,
    BLUGE_USEARCH_METRIC_COSINE = 3
};

typedef struct bluge_usearch_index bluge_usearch_index;

bluge_usearch_index *bluge_usearch_index_create(
    size_t dimensions,
    uint32_t metric,
    size_t connectivity,
    size_t expansion_add,
    size_t expansion_search);
bluge_usearch_index *bluge_usearch_index_open(const char *path);
void bluge_usearch_index_destroy(bluge_usearch_index *handle);
size_t bluge_usearch_index_dimensions(bluge_usearch_index *handle);
size_t bluge_usearch_index_size(bluge_usearch_index *handle);
size_t bluge_usearch_index_serialized_length(bluge_usearch_index *handle);
int32_t bluge_usearch_index_save_buffer(
    bluge_usearch_index *handle,
    uint8_t *output,
    size_t capacity);
bluge_usearch_index *bluge_usearch_index_open_buffer(
    const uint8_t *data,
    size_t length);
int32_t bluge_usearch_index_reserve(bluge_usearch_index *handle, size_t capacity);
/* ABI 4: reserves member capacity and grows the per-index thread pool that
 * bounds concurrent searches and insertions. Must not overlap other
 * operations on the handle. The effective member capacity never shrinks
 * below the index's current reserved capacity. */
bool bluge_usearch_index_reserve_threads(
    bluge_usearch_index *handle,
    size_t capacity,
    size_t threads);
int32_t bluge_usearch_index_add(
    bluge_usearch_index *handle,
    uint64_t key,
    const float *data,
    size_t len);
int32_t bluge_usearch_index_add_batch(
    bluge_usearch_index *handle,
    const uint64_t *keys,
    const float *vectors,
    size_t count,
    size_t dimensions);
int32_t bluge_usearch_index_get(
    bluge_usearch_index *handle,
    uint64_t key,
    float *output,
    size_t capacity,
    size_t *out_count);
int32_t bluge_usearch_index_remove(bluge_usearch_index *handle, uint64_t key);
int32_t bluge_usearch_index_remove_batch(
    bluge_usearch_index *handle,
    const uint64_t *keys,
    size_t count);
int32_t bluge_usearch_index_compact(bluge_usearch_index *handle);
int32_t bluge_usearch_index_save(bluge_usearch_index *handle, const char *path);
int32_t bluge_usearch_index_search(
    bluge_usearch_index *handle,
    const float *query,
    size_t query_len,
    size_t count,
    uint64_t *out_keys,
    float *out_distances,
    size_t *out_count);
int32_t bluge_usearch_index_search_filtered(
    bluge_usearch_index *handle,
    const float *query,
    size_t query_len,
    size_t count,
    const uint64_t *allowed_keys,
    size_t allowed_count,
    uint64_t *out_keys,
    float *out_distances,
    size_t *out_count);

/* Returns the required byte count. Copies at most capacity bytes. */
size_t bluge_usearch_index_last_error(
    bluge_usearch_index *handle,
    uint8_t *output,
    size_t capacity);

#ifdef __cplusplus
}
#endif

#endif
