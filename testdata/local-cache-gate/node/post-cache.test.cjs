const test = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const { spawn } = require('node:child_process');
const path = require('node:path');
const { savePost, verifyPost } = require('./post-cache');

test('post-handler cache writes restore exact bytes and refuse foreign capabilities', async () => {
  const uploads = new Map(); const saved = new Map(); let foreign = false;
  const server = http.createServer(async (req, res) => {
    const url = new URL(req.url, endpoint); const chunks = []; for await (const chunk of req) chunks.push(chunk);
    const data = Buffer.concat(chunks);
    if (url.pathname.includes('.CacheService/')) {
      assert.equal(req.headers.authorization, 'Bearer controlled-fixture');
      const input = JSON.parse(data);
      if (url.pathname.endsWith('CreateCacheEntry')) {
        uploads.set(input.key, Buffer.alloc(0));
        res.end(JSON.stringify({ ok: true, signed_upload_url: foreign ? 'http://foreign.invalid/upload' : endpoint + 'upload/' + input.key }));
      } else if (url.pathname.endsWith('FinalizeCacheEntryUpload')) {
        saved.set(input.key, uploads.get(input.key)); res.end('{"ok":true}');
      } else {
        res.end(JSON.stringify({ ok: saved.has(input.key), signed_download_url: endpoint + 'download/' + input.key }));
      }
    } else if (req.method === 'PUT') {
      const key = url.pathname.split('/').pop();
      if (url.searchParams.get('comp') === 'block') {
        assert.equal(Buffer.from(url.searchParams.get('blockid'),'base64').length, 48); uploads.set(key, data);
      }
      res.writeHead(201).end();
    } else { res.end(saved.get(url.pathname.split('/').pop())); }
  });
  await new Promise(r => server.listen(0, '127.0.0.1', r));
  const endpoint = `http://127.0.0.1:${server.address().port}/`;
  const options = { endpoint, token: 'controlled-fixture', runId: '42', attempt: '1' };
  try {
    for (const kind of ['node','composite']) await savePost(kind, options);
    await new Promise((resolve, reject) => {
      const child = spawn(process.execPath, [path.join(__dirname, '../container/index.js'), 'post'], {
        cwd: path.join(__dirname, '../../..'),
        env: { ...process.env, EXPECTED_CACHE_URL: endpoint, ACTIONS_RESULTS_URL: endpoint, ACTIONS_CACHE_URL: endpoint,
          ACTIONS_CACHE_SERVICE_V2: 'true', ACTIONS_RUNTIME_TOKEN: options.token,
          PROBE_POST_CACHE_KIND: 'container', GITHUB_RUN_ID: options.runId, GITHUB_RUN_ATTEMPT: options.attempt },
        stdio: ['ignore', 'pipe', 'pipe'],
      });
      child.stdout.resume(); child.stderr.resume();
      child.on('error', reject); child.on('exit', code => code === 0 ? resolve() : reject(new Error('container post exited ' + code)));
    });
    await verifyPost(options);
    assert.equal(saved.size, 3);
    assert.deepEqual(saved.get('runscaler-post-42-1-container'), Buffer.from('runscaler-post-cache:container\n'));
    saved.set('runscaler-post-42-1-container', Buffer.from('corrupt'));
    await assert.rejects(verifyPost(options), /restored post payload/);
    foreign = true;
    await assert.rejects(savePost('node', options), /local cache origin/);
  } finally { server.close(); }
});
