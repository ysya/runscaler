const assert = require('node:assert/strict');
(async () => {
  const expected = process.env.EXPECTED_CACHE_URL;
  assert.ok(expected, 'verified endpoint required');
  assert.equal(process.env.ACTIONS_RESULTS_URL, expected, 'container Results endpoint');
  assert.equal(String(process.env.ACTIONS_CACHE_SERVICE_V2).toLowerCase(), 'true', 'explicit container v2 mode required');
  assert.ok(process.env.ACTIONS_RUNTIME_TOKEN, 'container runtime token required');
  const response = await fetch(new URL('twirp/github.actions.results.api.v1.CacheService/GetCacheEntryDownloadURL', expected), {
    method: 'POST', redirect: 'error', signal: AbortSignal.timeout(10000),
    headers: { 'content-type': 'application/json', authorization: 'Bearer ' + process.env.ACTIONS_RUNTIME_TOKEN },
    body: JSON.stringify({ key: 'missing-container-' + require('node:crypto').randomUUID(), version: 'runscaler-gate-v1' }),
  });
  assert.equal(response.status, 200, 'container runtime authentication');
  assert.equal((await response.json()).ok, false, 'random container key must miss');
  if (process.argv[2] === 'post' && process.env.PROBE_POST_CACHE_KIND) {
    await require(require('node:path').resolve('testdata/local-cache-gate/node/post-cache.js')).savePost(
      process.env.PROBE_POST_CACHE_KIND, { endpoint: expected, token: process.env.ACTIONS_RUNTIME_TOKEN,
        runId: process.env.GITHUB_RUN_ID, attempt: process.env.GITHUB_RUN_ATTEMPT });
  }
  console.log('container Results endpoint and runtime authentication verified');
})().catch(() => { console.error('Local cache container gate failed.'); process.exitCode = 1; });
