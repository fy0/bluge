// Validate and archive the small result files, never the generated indices.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';

const [inputArg, outputArg, ...additionalInputs] = process.argv.slice(2);
assert(inputArg && outputArg, 'usage: node docs/summarize-collection-benchmark.mjs INPUT NEW_OUTPUT [ADDITIONAL_INPUT...]');
const inputs = [inputArg, ...additionalInputs].map(p => path.resolve(p));
const output = path.resolve(outputArg);
assert(!fs.existsSync(output), 'output already exists');
const read = file => JSON.parse(fs.readFileSync(file, 'utf8'));
const sum = (items, key) => items.reduce((total, item) => total + item[key], 0);
const peak = (items, key) => Math.max(0, ...items.map(item => item[key]));
const cases = [];
let signature;
let querySignature;
for (const backend of ['embedded', 'sidecar', 'collection']) {
  for (const models of [1, 2]) {
    const name = `${backend}-${models}`;
    const source = inputs.findLast(dir => fs.existsSync(path.join(dir, name, 'report.json')));
    assert(source, `missing case ${name}`);
    const root = path.join(source, name);
    const report = read(path.join(root, 'report.json'));
    const c = report.config;
    const finalQuery = report.FinalQuery;
    const currentQuerySignature = JSON.stringify([finalQuery.clock, finalQuery.queries_prepared_before_timing]);
    querySignature ??= currentQuerySignature;
    assert.equal(currentQuerySignature, querySignature, `${name}: incomparable final query clocks or input preparation`);
    assert.equal(c.Backend, backend);
    assert.equal(c.Models, models);
    const currentSignature = JSON.stringify([c.Documents, c.Dimensions, c.Batch, c.Rounds,
      c.Queries, c.Seed, c.Options, report.LibrarySHA256, report.GOMAXPROCS]);
    signature ??= currentSignature;
    assert.equal(currentSignature, signature, `${name}: incomparable configuration`);
    const builds = report.steps.filter(s => s.phase === 'build');
    const updates = report.steps.filter(s => s.phase === 'update');
    assert.equal(builds.length, Math.ceil(c.Documents / c.Batch));
    assert.equal(updates.length, c.Rounds);
    assert.equal(sum(builds, 'documents'), c.Documents);
    assert(updates.every(s => s.documents === Math.floor(c.Documents / 100)));
    const verifyBytes = bytes => {
      const components = Object.entries(bytes).filter(([key]) => key !== 'total_bytes');
      assert.equal(components.reduce((n, [, value]) => n + value, 0), bytes.total_bytes);
      assert.equal(bytes.temporary_bytes, 0, `${name}: residual temporary files`);
    };
    verifyBytes(report.InitialBytes);
    verifyBytes(report.FinalBytes);
    for (const [q, segments, deleted] of [
      [report.InitialQuery, builds.length, 0],
      [finalQuery, builds.length + updates.length, sum(updates, 'documents')],
    ]) {
      assert.equal(q.segments, segments);
      assert.equal(q.deleted_segment_rows, deleted);
      assert.equal(q.resident_query_ms_by_model.length, models);
      assert(q.resident_query_ms_by_model.every(samples => samples.length === c.Queries * 3));
      assert(q.recall_at_10_per_query.every(samples => samples.length === c.Queries));
      assert(q.recall_at_10_by_model.every(value => value >= 0 && value <= 1));
      if (backend !== 'embedded') {
        assert.deepEqual(q.verified_native_rows_by_model, Array(models).fill(c.Documents));
      }
    }
    const retention = [];
    if (backend === 'collection') {
      assert.equal(report.pin_probe.length, 3);
      verifyBytes(report.released_bytes);
      for (let model = 0; model < models; model++) {
        const dir = path.join(root, 'data', 'vectors', `space-model-${model}`);
        const catalog = read(path.join(dir, 'spec.json'));
        const files = fs.readdirSync(dir);
        const lastWorkloadGeneration = builds.length + updates.length;
        assert.equal(report.retained_generations, 1);
        assert.deepEqual(catalog.generations, [lastWorkloadGeneration + 3]);
        assert(!files.some(f => f.startsWith('.pending-') || f.startsWith('.retired-')));
        for (const prefix of ['index-', 'metadata-', 'commit-']) {
          assert.equal(files.filter(f => f.startsWith(prefix)).length, 1);
        }
        const generations = catalog.generations.map(generation => {
          const suffix = String(generation).padStart(20, '0');
          const metadata = read(path.join(dir, `metadata-${suffix}.json`));
          assert.equal(Object.keys(metadata.revisions).length, c.Documents);
          return {
            generation,
            rows: Object.keys(metadata.revisions).length,
            index_bytes: fs.statSync(path.join(dir, `index-${suffix}.usearch`)).size,
            mapping_bytes: fs.statSync(path.join(dir, `metadata-${suffix}.json`)).size,
          };
        });
        retention.push({ space: catalog.spec.id, generations });
      }
    }
    cases.push({
      name, root, report,
      summary: {
        name, build_seconds: report.BuildMS / 1000, update_seconds: report.UpdatesMS / 1000,
        mean_update_seconds: report.UpdatesMS / c.Rounds / 1000,
        build_peak_bytes: peak(builds, 'sampled_peak_bytes'),
        update_peak_bytes: peak(updates, 'sampled_peak_bytes'),
        build_temporary_peak_bytes: peak(builds, 'sampled_peak_temporary_bytes'),
        update_temporary_peak_bytes: peak(updates, 'sampled_peak_temporary_bytes'),
        raw_float32_bytes: c.Documents * c.Dimensions * 4 * models,
        initial: report.InitialBytes, final: report.FinalBytes,
        initial_query: report.InitialQuery, final_query: finalQuery,
        ...(backend === 'collection' ? {
          // Counterfactual payload total, not a separately timed no-GC run.
          retain_all_derived_vector_bytes: sum(report.steps.map(s => s.bytes), 'current_vector_bytes'),
          retain_all_derived_mapping_bytes: sum(report.steps.map(s => s.bytes), 'current_mapping_bytes'),
          pinned: report.pin_probe.at(-1).bytes,
          pin_peak_bytes: peak(report.pin_probe, 'sampled_peak_bytes'),
          pin_temporary_peak_bytes: peak(report.pin_probe, 'sampled_peak_temporary_bytes'),
          released: report.released_bytes,
          pin_query_ms: report.pinned_lazy_query_ms,
          release_ms: report.pin_release_maintenance_ms,
          retention,
        } : {}),
      },
    });
  }
}

fs.mkdirSync(output, { recursive: true });
for (const { name, root } of cases) {
  const dest = path.join(output, name);
  fs.mkdirSync(dest);
  for (const file of fs.readdirSync(root).filter(f => f.endsWith('.json'))) {
    fs.copyFileSync(path.join(root, file), path.join(dest, file));
  }
}
for (const [index, input] of inputs.entries()) {
  for (const suffix of ['environment.txt', 'log']) {
    fs.copyFileSync(`${input}.${suffix}`, path.join(output, `run-${index + 1}.${suffix}`));
  }
}
fs.writeFileSync(path.join(output, 'provenance.json'), JSON.stringify(cases.map(c => ({ name: c.name, source: c.root })), null, 2) + '\n');
fs.writeFileSync(path.join(output, 'summary.json'), JSON.stringify(cases.map(c => c.summary), null, 2) + '\n');
const mib = n => (n / 1048576).toFixed(2);
const tables = [
  '| Case | Build s | Updates s | Initial MiB | Final MiB | Update peak MiB | Temporary peak MiB |',
  '| --- | ---: | ---: | ---: | ---: | ---: | ---: |',
  ...cases.map(({ summary: s }) => `| ${s.name} | ${s.build_seconds.toFixed(2)} | ${s.update_seconds.toFixed(2)} | ${mib(s.initial.total_bytes)} | ${mib(s.final.total_bytes)} | ${mib(s.update_peak_bytes)} | ${mib(s.update_temporary_peak_bytes)} |`),
];
tables.push('', 'Final disk breakdown (MiB):', '',
  '| Case | Text | Current native | History native | Current mapping | History mapping | Control | Temporary |',
  '| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |');
for (const { summary: s } of cases) {
  const b = s.final;
  tables.push(`| ${s.name} | ${[b.text_bytes, b.current_vector_bytes, b.history_vector_bytes,
    b.current_mapping_bytes, b.history_mapping_bytes, b.control_bytes, b.temporary_bytes].map(mib).join(' | ')} |`);
}
tables.push('', 'Fresh-process open and per-model queries (ms); arrays are model 0 / model 1:', '',
  '| Case | Phase | Clock | Open | First query | p50 | p95 | Recall@10 |',
  '| --- | --- | --- | ---: | --- | --- | --- | --- |');
const vector = (values, digits = 3) => values.map(n => n.toFixed(digits)).join(' / ');
for (const { summary: s } of cases) {
  for (const phase of ['initial', 'final']) {
    const q = s[`${phase}_query`];
    tables.push(`| ${s.name} | ${phase} | ${q.clock} | ${q.open_ms.toFixed(3)} | ${vector(q.first_query_ms_by_model)} | ${vector(q.p50_ms_by_model)} | ${vector(q.p95_ms_by_model)} | ${vector(q.recall_at_10_by_model, 5)} |`);
  }
}
tables.push('', 'Collection lazy reader retention probe (MiB, except ms columns):', '',
  '| Case | Pinned total | Peak total | Peak temporary | Released total | Lazy query ms | Release GC ms | Retain-all derived native + mapping |',
  '| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |');
for (const { summary: s } of cases.filter(c => c.summary.retention)) {
  tables.push(`| ${s.name} | ${mib(s.pinned.total_bytes)} | ${mib(s.pin_peak_bytes)} | ${mib(s.pin_temporary_peak_bytes)} | ${mib(s.released.total_bytes)} | ${s.pin_query_ms.toFixed(3)} | ${s.release_ms.toFixed(3)} | ${mib(s.retain_all_derived_vector_bytes + s.retain_all_derived_mapping_bytes)} |`);
}
fs.writeFileSync(path.join(output, 'tables.md'), tables.join('\n') + '\n');
console.log(tables.join('\n'));
console.log(`Validated and archived ${cases.length} cases at ${output}`);
