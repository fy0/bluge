use std::collections::HashSet;
use std::ffi::{CStr, CString, c_char};
use std::panic::{AssertUnwindSafe, catch_unwind};
use std::ptr;
use std::slice;
use std::sync::Mutex;
use std::sync::OnceLock;

use usearch::{Index, IndexOptions, MetricKind, ScalarKind};

const METRIC_L2: u32 = 1;
const METRIC_DOT: u32 = 2;
const METRIC_COSINE: u32 = 3;
const ABI_VERSION: u32 = 2;

static HARDWARE_COMPILED: OnceLock<CString> = OnceLock::new();
static HARDWARE_AVAILABLE: OnceLock<CString> = OnceLock::new();

pub struct IndexHandle {
    index: Index,
    last_error: Mutex<Vec<u8>>,
}

impl IndexHandle {
    fn new(index: Index) -> Self {
        Self {
            index,
            last_error: Mutex::new(Vec::new()),
        }
    }

    fn clear_error(&self) {
        if let Ok(mut error) = self.last_error.lock() {
            error.clear();
        }
    }

    fn set_error(&self, error: impl ToString) -> i32 {
        if let Ok(mut message) = self.last_error.lock() {
            *message = error.to_string().into_bytes();
        }
        -1
    }
}

fn metric_kind(metric: u32) -> Result<MetricKind, String> {
    match metric {
        METRIC_L2 => Ok(MetricKind::L2sq),
        METRIC_DOT => Ok(MetricKind::IP),
        METRIC_COSINE => Ok(MetricKind::Cos),
        _ => Err(format!("unsupported metric {metric}")),
    }
}

fn c_path(path: *const c_char) -> Result<String, String> {
    if path.is_null() {
        return Err("path is null".to_owned());
    }
    // C callers own the path buffer for the duration of this call.
    let path = unsafe { CStr::from_ptr(path) };
    path.to_str()
        .map(str::to_owned)
        .map_err(|_| "path is not valid UTF-8".to_owned())
}

fn vector<'a>(data: *const f32, len: usize, dimensions: usize) -> Result<&'a [f32], String> {
    if len != dimensions {
        return Err(format!(
            "vector has {len} dimensions, expected {dimensions}"
        ));
    }
    if data.is_null() {
        return Err("vector data is null".to_owned());
    }
    // The caller keeps the input slice alive until the FFI call returns.
    Ok(unsafe { slice::from_raw_parts(data, len) })
}

fn handle_ref<'a>(handle: *mut IndexHandle) -> Result<&'a IndexHandle, String> {
    if handle.is_null() {
        return Err("index handle is null".to_owned());
    }
    // The handle is created by this library and remains owned by the caller.
    Ok(unsafe { &*handle })
}

fn status(
    handle: *mut IndexHandle,
    operation: impl FnOnce(&IndexHandle) -> Result<(), String>,
) -> i32 {
    let handle = match handle_ref(handle) {
        Ok(handle) => handle,
        Err(_) => return -1,
    };
    handle.clear_error();
    match catch_unwind(AssertUnwindSafe(|| operation(handle))) {
        Ok(Ok(())) => 0,
        Ok(Err(error)) => handle.set_error(error),
        Err(_) => handle.set_error("native index operation panicked"),
    }
}

#[unsafe(no_mangle)]
pub extern "C" fn bluge_usearch_abi_version() -> u32 {
    ABI_VERSION
}

/// Returns a pointer to a static, comma-separated list of ISAs compiled into
/// this binary. The pointer remains valid for the lifetime of the library.
#[unsafe(no_mangle)]
pub extern "C" fn bluge_usearch_hardware_acceleration_compiled() -> *const c_char {
    HARDWARE_COMPILED
        .get_or_init(|| {
            CString::new(usearch::hardware_acceleration_compiled())
                .expect("USearch hardware acceleration list contains NUL")
        })
        .as_ptr()
}

/// Returns a pointer to a static, comma-separated list of ISAs available on
/// the current CPU. The pointer remains valid for the lifetime of the library.
#[unsafe(no_mangle)]
pub extern "C" fn bluge_usearch_hardware_acceleration_available() -> *const c_char {
    HARDWARE_AVAILABLE
        .get_or_init(|| {
            CString::new(usearch::hardware_acceleration_available())
                .expect("USearch hardware availability list contains NUL")
        })
        .as_ptr()
}

#[unsafe(no_mangle)]
pub extern "C" fn bluge_usearch_index_create(
    dimensions: usize,
    metric: u32,
    connectivity: usize,
    expansion_add: usize,
    expansion_search: usize,
) -> *mut IndexHandle {
    let result = catch_unwind(AssertUnwindSafe(|| {
        let options = IndexOptions {
            dimensions,
            metric: metric_kind(metric)?,
            quantization: ScalarKind::F32,
            connectivity,
            expansion_add,
            expansion_search,
            multi: false,
        };
        Index::new(&options)
            .map(IndexHandle::new)
            .map_err(|error| error.to_string())
    }));

    match result {
        Ok(Ok(handle)) => Box::into_raw(Box::new(handle)),
        _ => ptr::null_mut(),
    }
}

#[unsafe(no_mangle)]
pub extern "C" fn bluge_usearch_index_open(path: *const c_char) -> *mut IndexHandle {
    let result = catch_unwind(AssertUnwindSafe(|| {
        let path = c_path(path)?;
        Index::restore(&path)
            .map(IndexHandle::new)
            .map_err(|error| error.to_string())
    }));

    match result {
        Ok(Ok(handle)) => Box::into_raw(Box::new(handle)),
        _ => ptr::null_mut(),
    }
}

/// # Safety
///
/// `handle` must be a pointer returned by this library and must not be used
/// again after destruction.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn bluge_usearch_index_destroy(handle: *mut IndexHandle) {
    if !handle.is_null() {
        // Reclaims the allocation created by index_create/index_open.
        unsafe { drop(Box::from_raw(handle)) };
    }
}

#[unsafe(no_mangle)]
pub extern "C" fn bluge_usearch_index_dimensions(handle: *mut IndexHandle) -> usize {
    match handle_ref(handle) {
        Ok(handle) => handle.index.dimensions(),
        Err(_) => 0,
    }
}

#[unsafe(no_mangle)]
pub extern "C" fn bluge_usearch_index_size(handle: *mut IndexHandle) -> usize {
    match handle_ref(handle) {
        Ok(handle) => handle.index.size(),
        Err(_) => 0,
    }
}

#[unsafe(no_mangle)]
pub extern "C" fn bluge_usearch_index_serialized_length(handle: *mut IndexHandle) -> usize {
    match handle_ref(handle) {
        Ok(handle) => handle.index.serialized_length(),
        Err(_) => 0,
    }
}

/// # Safety
///
/// `handle` must be live. When `output` is non-null, it must point to a
/// writable buffer of at least `capacity` bytes.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn bluge_usearch_index_save_buffer(
    handle: *mut IndexHandle,
    output: *mut u8,
    capacity: usize,
) -> i32 {
    status(handle, |handle| {
        let required = handle.index.serialized_length();
        if capacity < required {
            return Err(format!(
                "serialization buffer has {capacity} bytes, needs {required}"
            ));
        }
        if required != 0 && output.is_null() {
            return Err("serialization output buffer is null".to_owned());
        }
        let output = unsafe { slice::from_raw_parts_mut(output, capacity) };
        handle
            .index
            .save_to_buffer(output)
            .map_err(|error| error.to_string())
    })
}

#[unsafe(no_mangle)]
pub unsafe extern "C" fn bluge_usearch_index_open_buffer(
    data: *const u8,
    length: usize,
) -> *mut IndexHandle {
    if data.is_null() || length == 0 {
        return ptr::null_mut();
    }
    let data = unsafe { slice::from_raw_parts(data, length) };
    let result = catch_unwind(AssertUnwindSafe(|| {
        Index::restore_from_buffer(data)
            .map(IndexHandle::new)
            .map_err(|error| error.to_string())
    }));
    match result {
        Ok(Ok(handle)) => Box::into_raw(Box::new(handle)),
        _ => ptr::null_mut(),
    }
}

#[unsafe(no_mangle)]
pub extern "C" fn bluge_usearch_index_reserve(handle: *mut IndexHandle, capacity: usize) -> i32 {
    status(handle, |handle| {
        handle
            .index
            .reserve(capacity)
            .map_err(|error| error.to_string())
    })
}

#[unsafe(no_mangle)]
pub extern "C" fn bluge_usearch_index_add(
    handle: *mut IndexHandle,
    key: u64,
    data: *const f32,
    len: usize,
) -> i32 {
    status(handle, |handle| {
        let data = vector(data, len, handle.index.dimensions())?;
        handle
            .index
            .add(key, data)
            .map_err(|error| error.to_string())
    })
}

/// # Safety
///
/// `handle`, `output`, and `out_count` must remain valid for the duration of
/// this call. `output` must have room for at least `capacity` float values.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn bluge_usearch_index_get(
    handle: *mut IndexHandle,
    key: u64,
    output: *mut f32,
    capacity: usize,
    out_count: *mut usize,
) -> i32 {
    let handle = match handle_ref(handle) {
        Ok(handle) => handle,
        Err(_) => return -1,
    };
    if out_count.is_null() {
        return handle.set_error("output count is null");
    }
    unsafe { *out_count = 0 };
    handle.clear_error();
    let result = catch_unwind(AssertUnwindSafe(|| {
        if capacity != handle.index.dimensions() {
            return Err(format!(
                "vector output has {capacity} dimensions, expected {}",
                handle.index.dimensions()
            ));
        }
        if capacity != 0 && output.is_null() {
            return Err("vector output buffer is null".to_owned());
        }
        let output = unsafe { slice::from_raw_parts_mut(output, capacity) };
        let count = handle
            .index
            .get(key, output)
            .map_err(|error| error.to_string())?;
        unsafe { *out_count = count };
        Ok(())
    }));
    match result {
        Ok(Ok(())) => 0,
        Ok(Err(error)) => handle.set_error(error),
        Err(_) => handle.set_error("native index operation panicked"),
    }
}

#[unsafe(no_mangle)]
pub extern "C" fn bluge_usearch_index_remove(handle: *mut IndexHandle, key: u64) -> i32 {
    status(handle, |handle| {
        handle
            .index
            .remove(key)
            .map(|_| ())
            .map_err(|error| error.to_string())
    })
}

#[unsafe(no_mangle)]
pub extern "C" fn bluge_usearch_index_compact(handle: *mut IndexHandle) -> i32 {
    status(handle, |handle| {
        handle.index.compact().map_err(|error| error.to_string())
    })
}

#[unsafe(no_mangle)]
pub extern "C" fn bluge_usearch_index_save(handle: *mut IndexHandle, path: *const c_char) -> i32 {
    status(handle, |handle| {
        let path = c_path(path)?;
        handle.index.save(&path).map_err(|error| error.to_string())
    })
}

/// # Safety
///
/// `handle`, `query`, and the output buffers must remain valid for the
/// duration of this call. Output buffers must have room for `count` entries.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn bluge_usearch_index_search(
    handle: *mut IndexHandle,
    query: *const f32,
    query_len: usize,
    count: usize,
    out_keys: *mut u64,
    out_distances: *mut f32,
    out_count: *mut usize,
) -> i32 {
    let handle = match handle_ref(handle) {
        Ok(handle) => handle,
        Err(_) => return -1,
    };
    if out_count.is_null() {
        return handle.set_error("output count is null");
    }
    unsafe { *out_count = 0 };
    handle.clear_error();
    let result = catch_unwind(AssertUnwindSafe(|| {
        let query = vector(query, query_len, handle.index.dimensions())?;
        if count == 0 {
            return Err("search count must be greater than zero".to_owned());
        }
        let matches = handle
            .index
            .search(query, count)
            .map_err(|error| error.to_string())?;
        if matches.keys.len() != matches.distances.len() {
            return Err("native search returned mismatched result buffers".to_owned());
        }
        if matches.keys.len() > count {
            return Err("native search returned too many results".to_owned());
        }
        if !matches.keys.is_empty() && (out_keys.is_null() || out_distances.is_null()) {
            return Err("search output buffer is null".to_owned());
        }
        if !matches.keys.is_empty() {
            unsafe {
                ptr::copy_nonoverlapping(matches.keys.as_ptr(), out_keys, matches.keys.len());
                ptr::copy_nonoverlapping(
                    matches.distances.as_ptr(),
                    out_distances,
                    matches.distances.len(),
                );
                *out_count = matches.keys.len();
            }
        }
        Ok(())
    }));

    match result {
        Ok(Ok(())) => 0,
        Ok(Err(error)) => handle.set_error(error),
        Err(_) => handle.set_error("native index operation panicked"),
    }
}

/// Searches while admitting only keys present in `allowed_keys`.
///
/// # Safety
///
/// `handle`, `query`, `allowed_keys`, and the output buffers must remain valid
/// for the duration of this call. Output buffers must have room for `count`
/// entries, and `allowed_keys` must have room for `allowed_count` entries.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn bluge_usearch_index_search_filtered(
    handle: *mut IndexHandle,
    query: *const f32,
    query_len: usize,
    count: usize,
    allowed_keys: *const u64,
    allowed_count: usize,
    out_keys: *mut u64,
    out_distances: *mut f32,
    out_count: *mut usize,
) -> i32 {
    let handle = match handle_ref(handle) {
        Ok(handle) => handle,
        Err(_) => return -1,
    };
    if out_count.is_null() {
        return handle.set_error("output count is null");
    }
    unsafe { *out_count = 0 };
    handle.clear_error();
    let result = catch_unwind(AssertUnwindSafe(|| {
        let query = vector(query, query_len, handle.index.dimensions())?;
        if count == 0 {
            return Err("search count must be greater than zero".to_owned());
        }
        if allowed_count == 0 {
            return Ok(());
        }
        if allowed_keys.is_null() {
            return Err("allowed key buffer is null".to_owned());
        }
        let allowed_keys = unsafe { slice::from_raw_parts(allowed_keys, allowed_count) };
        let allowed_keys = allowed_keys.iter().copied().collect::<HashSet<_>>();
        let matches = handle
            .index
            .filtered_search(query, count, |key| allowed_keys.contains(&key))
            .map_err(|error| error.to_string())?;
        if matches.keys.len() != matches.distances.len() {
            return Err("native filtered search returned mismatched result buffers".to_owned());
        }
        if matches.keys.len() > count {
            return Err("native filtered search returned too many results".to_owned());
        }
        if !matches.keys.is_empty() && (out_keys.is_null() || out_distances.is_null()) {
            return Err("filtered search output buffer is null".to_owned());
        }
        if !matches.keys.is_empty() {
            unsafe {
                ptr::copy_nonoverlapping(matches.keys.as_ptr(), out_keys, matches.keys.len());
                ptr::copy_nonoverlapping(
                    matches.distances.as_ptr(),
                    out_distances,
                    matches.distances.len(),
                );
                *out_count = matches.keys.len();
            }
        }
        Ok(())
    }));

    match result {
        Ok(Ok(())) => 0,
        Ok(Err(error)) => handle.set_error(error),
        Err(_) => handle.set_error("native filtered search operation panicked"),
    }
}

/// # Safety
///
/// `handle` must be a live pointer returned by this library. When `output` is
/// non-null, it must point to a writable buffer of at least `capacity` bytes.
#[unsafe(no_mangle)]
pub unsafe extern "C" fn bluge_usearch_index_last_error(
    handle: *mut IndexHandle,
    output: *mut u8,
    capacity: usize,
) -> usize {
    let handle = match handle_ref(handle) {
        Ok(handle) => handle,
        Err(_) => return 0,
    };
    let error = match handle.last_error.lock() {
        Ok(error) => error,
        Err(_) => return 0,
    };
    if !output.is_null() && capacity != 0 {
        let count = error.len().min(capacity);
        unsafe { ptr::copy_nonoverlapping(error.as_ptr(), output, count) };
    }
    error.len()
}
