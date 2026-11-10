const fs = require('node:fs');
const path = require('node:path');
const file = path.join(process.env.RUNNER_TEMP, 'runscaler-cache-metrics.jsonl');
const events = fs.readFileSync(file, 'utf8').trim().split('\n').filter(Boolean).map(JSON.parse);
const summary = { schema: 1, workload: process.env.PROBE_WORKLOAD, scenario: process.env.PROBE_SCENARIO || 'normal', phase: process.env.PROBE_PHASE,
  pair: process.env.PROBE_PAIR, total_ms: Date.now() - Number(process.env.STATE_started), events };
console.log('RUNSCALER_CACHE_REPORT=' + JSON.stringify(summary));
if (events.some(x => x.kind === 'blocked')) throw new Error('Foreign cache transfer blocked');
