const fs = require('node:fs');
const cp = require('node:child_process');
const path = require('node:path');
const crypto = require('node:crypto');
const root = __dirname;
const workload = process.env.PROBE_WORKLOAD;
const start = performance.now();
let value;
const run = (file, args, directory = workload) => cp.execFileSync(file, args, { cwd: path.join(root, directory), encoding: 'utf8' });
switch (workload) {
  case 'direct': {
    const dir = path.join(process.env.GITHUB_WORKSPACE, '.local-cache-probe');
    const file = path.join(dir, 'payload');
    const expected = Buffer.from(Array.from({ length: 300000 }, (_, i) => i % 251));
    if (process.env.PROBE_PHASE === 'warm' && !fs.existsSync(file)) throw new Error('restored file missing');
    if (fs.existsSync(file) && !fs.readFileSync(file).equals(expected)) throw new Error('restored hash mismatch');
    fs.mkdirSync(dir, { recursive: true }); fs.writeFileSync(file, expected);
    value = fs.readFileSync(file); break;
  }
  case 'go': value = run('go', ['run', '-mod=readonly', '.']); break;
  case 'node': run('npm', ['ci', '--ignore-scripts', ...(process.env.PROBE_PHASE === 'warm' ? ['--offline'] : [])]); value = run('node', ['index.cjs']); break;
  case 'gradle': run('gradle', ['--no-daemon', '--build-cache', 'classes']); value = run('java', ['-cp', 'build/classes/java/main', 'Probe']); break;
  case 'rust': {
    if (process.env.PROBE_PHASE === 'warm') {
      const cache = path.join(process.env.CARGO_HOME, 'registry', 'cache');
      const archive = fs.readdirSync(cache).map(name => path.join(cache, name, 'itoa-1.0.15.crate')).find(file => fs.existsSync(file));
      if (!archive || crypto.createHash('sha256').update(fs.readFileSync(archive)).digest('hex') !== '4a5f13b858c8d314ee3e8f639011f7ccefe71f97f96e50151fb991f267928e2c') throw new Error('restored Rust crate archive mismatch');
    }
    value = run('cargo', ['run', '--locked', '--quiet']); break;
  }
  default: throw new Error('unknown workload');
}
if (workload !== 'direct' && value !== 'runscaler-local-cache\n') throw new Error('fixture output mismatch');
fs.appendFileSync(process.env.CACHE_OBSERVER_FILE, JSON.stringify({ schema: 1, kind: 'build', ms: Math.round(performance.now() - start), sha256: crypto.createHash('sha256').update(value).digest('hex') }) + '\n');
console.log('Fixture content checksum verified');
