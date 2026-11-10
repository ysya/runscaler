const assert = require('node:assert/strict');
const fs = require('node:fs');
const crypto = require('node:crypto');

module.exports = async function check(post = false) {
  const expected = process.env.STATE_expected_cache_url || process.env.EXPECTED_CACHE_URL || process.env.CUSTOM_ACTIONS_RESULTS_URL;
  assert.ok(expected, 'an explicit local endpoint is required');
  const local = new URL(expected);
  assert.ok(['http:', 'https:'].includes(local.protocol), 'HTTP(S) endpoint required');
  assert.equal(local.username + local.password + local.search + local.hash, '', 'endpoint must not contain credentials or query parameters');
  assert.ok(!local.hostname.endsWith('githubusercontent.com') && !local.hostname.endsWith('github.com'), 'GitHub cannot be the cache endpoint');
  assert.equal(process.env.ACTIONS_RESULTS_URL, expected, 'runner overwrote local Results endpoint');
  assert.equal(String(process.env.ACTIONS_CACHE_SERVICE_V2).toLowerCase(), 'true', 'v2 required before any cache action runs');
  assert.ok(process.env.ACTIONS_RUNTIME_TOKEN, 'runtime token required');
  const response = await fetch(new URL('twirp/github.actions.results.api.v1.CacheService/GetCacheEntryDownloadURL', expected), {
    method: 'POST', redirect: 'error', signal: AbortSignal.timeout(10000),
    headers: { 'content-type': 'application/json', authorization: 'Bearer ' + process.env.ACTIONS_RUNTIME_TOKEN },
    body: JSON.stringify({ key: 'runscaler-missing-' + crypto.randomUUID(), version: 'runscaler-gate-v1', restore_keys: [] }),
  });
  assert.equal(response.status, 200, 'real runtime token must authenticate on the local service');
  assert.equal((await response.json()).ok, false, 'random probe key must miss');
  const postKind = process.env.STATE_post_cache_kind || process.env.PROBE_POST_CACHE_KIND;
  const postOptions = { endpoint: expected, token: process.env.ACTIONS_RUNTIME_TOKEN,
    runId: process.env.GITHUB_RUN_ID, attempt: process.env.GITHUB_RUN_ATTEMPT };
  if (post && postKind) await require('./post-cache').savePost(postKind, postOptions);
  if (!post && process.env.PROBE_POST_CACHE_VERIFY === 'true') await require('./post-cache').verifyPost(postOptions);
  if (!post) {
    if (postKind) fs.appendFileSync(process.env.GITHUB_STATE, 'post_cache_kind=' + postKind + '\n');
    fs.appendFileSync(process.env.GITHUB_STATE, 'expected_cache_url=' + expected + '\n');
    fs.appendFileSync(process.env.GITHUB_ENV, 'EXPECTED_CACHE_URL=' + expected + '\n');
  }
  console.log(post ? 'local Results endpoint and runtime token verified in post-step' : 'local Results endpoint, v2 mode and runtime token verified');
};
