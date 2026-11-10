// Synthetic cache-v2 writes from actual action post handlers. Never log tokens/URLs.
const assert = require('node:assert/strict');
const { randomUUID } = require('node:crypto');
const kinds = ['node', 'composite', 'container'];
const version = 'runscaler-post-save-v1';
function entry(kind, options) {
  assert.ok(kinds.includes(kind), 'supported post handler');
  assert.ok(options.runId && options.attempt, 'GitHub run identity required');
  return { key: `runscaler-post-${options.runId}-${options.attempt}-${kind}`, version };
}
function payload(kind) { return Buffer.from(`runscaler-post-cache:${kind}\n`); }
function capability(value, options) {
  const url = new URL(value);
  assert.equal(url.origin, new URL(options.endpoint).origin, 'local cache origin required');
  return url;
}
async function rpc(method, body, options) {
  const response = await fetch(new URL('twirp/github.actions.results.api.v1.CacheService/' + method, options.endpoint), {
    method: 'POST', headers: { 'content-type': 'application/json', authorization: 'Bearer ' + options.token },
    body: JSON.stringify(body), redirect: 'error', signal: AbortSignal.timeout(10000),
  });
  assert.equal(response.status, 200, 'post cache RPC status');
  const result = await response.json(); assert.equal(result.ok, true, 'post cache RPC succeeds'); return result;
}
async function savePost(kind, options) {
  const identity = entry(kind, options); const data = payload(kind);
  const reservation = await rpc('CreateCacheEntry', identity, options);
  const upload = capability(reservation.signed_upload_url, options);
  const id = Buffer.from(randomUUID() + '000000000000').toString('base64');
  const block = new URL(upload); block.searchParams.set('comp', 'block'); block.searchParams.set('blockid', id);
  const sent = await fetch(block, { method: 'PUT', body: data, redirect: 'error', signal: AbortSignal.timeout(10000) });
  assert.equal(sent.status, 201, 'post upload block'); await sent.arrayBuffer();
  const list = new URL(upload); list.searchParams.set('comp', 'blocklist');
  const committed = await fetch(list, { method: 'PUT', headers: { 'content-type': 'application/xml' },
    body: '<?xml version="1.0" encoding="utf-8"?><BlockList><Latest>' + id + '</Latest></BlockList>',
    redirect: 'error', signal: AbortSignal.timeout(10000) });
  assert.equal(committed.status, 201, 'post commit block'); await committed.arrayBuffer();
  await rpc('FinalizeCacheEntryUpload', { ...identity, size_bytes: String(data.length) }, options);
  console.log(kind + ' post cache save verified');
}
async function verifyPost(options) {
  for (const kind of kinds) {
    const result = await rpc('GetCacheEntryDownloadURL', entry(kind, options), options);
    const response = await fetch(capability(result.signed_download_url, options), { redirect: 'error', signal: AbortSignal.timeout(10000) });
    assert.equal(response.status, 200, 'post payload download');
    assert.deepEqual(Buffer.from(await response.arrayBuffer()), payload(kind), 'restored post payload');
    console.log(kind + ' post cache restore checksum verified');
  }
}
module.exports = { savePost, verifyPost };
