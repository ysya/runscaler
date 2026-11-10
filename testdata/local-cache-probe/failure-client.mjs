// Run only in an owned isolated fixture with its disposable controlled issuer.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import crypto from 'node:crypto';
import { spawnSync } from 'node:child_process';
const ref = '55cc8345863c7cc4c66a329aec7e433d2d1c52a9';
const root = fs.mkdtempSync(path.join(os.tmpdir(), 'cache-failure-'));
try {
  for (const [name, source] of [['save', 'save-only'], ['restore', 'restore-only']]) {
    const response = await fetch(`https://raw.githubusercontent.com/actions/cache/${ref}/dist/${source}/index.js`);
    assert.equal(response.status, 200, 'pinned action download');
    fs.writeFileSync(path.join(root, name + '.mjs'), Buffer.from(await response.arrayBuffer()));
  }
  const tokenResponse = await fetch(process.env.PROBE_TOKEN_URL, { method: 'POST' });
  assert.equal(tokenResponse.status, 200, 'controlled fixture token');
  const token = await tokenResponse.text();
  for (const { name, url, invalid_token } of JSON.parse(process.env.PROBE_FAILURE_URLS)) {
    const work = path.join(root, name); fs.mkdirSync(work);
    const data = path.join(work, 'payload'); fs.writeFileSync(data, crypto.randomBytes(2 * 1024 * 1024));
    const env = { ...process.env, ACTIONS_CACHE_URL: url, ACTIONS_RESULTS_URL: url, ACTIONS_CACHE_SERVICE_V2: 'true',
      ACTIONS_RUNTIME_TOKEN: invalid_token ? 'invalid-controlled-token' : token, INPUT_PATH: data, INPUT_KEY: 'failure-' + crypto.randomUUID(),
      GITHUB_REF: 'refs/heads/main', GITHUB_EVENT_NAME: 'push', GITHUB_WORKSPACE: work, RUNNER_TEMP: work,
      GITHUB_OUTPUT: path.join(work, 'outputs'), GITHUB_STATE: path.join(work, 'state'),
      EXPECTED_CACHE_URL: url, CACHE_OBSERVER_FILE: path.join(work, 'metrics.jsonl'), NODE_OPTIONS: '--require ' + process.env.PROBE_OBSERVER_PATH };
    fs.writeFileSync(env.GITHUB_OUTPUT, ''); fs.writeFileSync(env.GITHUB_STATE, '');
    const invoke = action => spawnSync(process.execPath, [path.join(root, action + '.mjs')], { env, cwd: work, encoding: 'utf8', timeout: 180000 });
    const saved = invoke('save');
    const saveOutput = saved.stdout + saved.stderr;
    if (saved.status !== 0) console.error(saveOutput.replaceAll(token, '[token]').replace(/https?:\/\/[^\s"<>]+/g, '[url]').slice(-5000));
    assert.equal(saved.status, 0, 'cache save failure remains optional');
    assert.ok(!saveOutput.includes('Cache saved with key:'), 'failed save must not claim a cache entry');
    fs.unlinkSync(data);
    const restored = invoke('restore');
    assert.equal(restored.status, 0, 'ordinary cache restore failure remains optional');
    assert.ok(!fs.existsSync(data), 'failed save must not restore a payload');
    const events = fs.existsSync(env.CACHE_OBSERVER_FILE) ? fs.readFileSync(env.CACHE_OBSERVER_FILE, 'utf8').trim().split('\n').filter(Boolean).map(JSON.parse) : [];
    assert.ok(!events.some(x => x.kind === 'blocked'), 'no foreign cache request attempted');
    console.log(JSON.stringify({ schema: 1, scenario: name, save_exit: saved.status, restore_exit: restored.status,
      warning_observed: /warning|failed|error|unable/i.test(saveOutput), saved: false, restored: false, cloud_fallback: false,
      note: 'controlled issuer, actual pinned actions/cache save and restore; other language helpers need their own evidence' }));
  }
} finally { fs.rmSync(root, { recursive: true, force: true }); }
