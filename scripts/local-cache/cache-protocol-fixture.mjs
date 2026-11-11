// Controlled issuer and protocol fixture. Run only on an isolated test network.
// Generates disposable RSA keys in memory; never prints tokens or signed URLs.
import assert from 'node:assert/strict';
import { generateKeyPairSync, randomUUID, sign, createHash } from 'node:crypto';
import { createServer, request as httpRequest } from 'node:http';
import { request as httpsRequest } from 'node:https';

const base = process.env.PROBE_CACHE_URL ?? 'http://gate-server:3000/';
const issuer = process.env.PROBE_ISSUER_URL ?? 'http://gate-client:4080';
const issuerBindHost = process.env.PROBE_BIND_HOST ?? '0.0.0.0';
const unknownDownloadStatus = Number(process.env.PROBE_UNKNOWN_DOWNLOAD_STATUS ?? 404);
const { privateKey, publicKey } = generateKeyPairSync('rsa', { modulusLength: 2048 });
const kid = randomUUID();
const jwk = { ...publicKey.export({ format: 'jwk' }), kid, alg: 'RS256', use: 'sig' };
const service = 'twirp/github.actions.results.api.v1.CacheService/';
let key = 'runscaler-controlled-' + randomUUID();
const version = 'runscaler-protocol-fixture-v1';
const payload = Buffer.from(Array.from({ length: 300000 }, (_, i) => i % 251));
let artifactRequests = 0;
let busy = false;
const cancelledKeys = [];

function token(overrides = {}, signingKey = privateKey) {
  const now = Math.floor(Date.now() / 1000);
  const claims = { iss: issuer, exp: now + 600, nbf: now - 1, aud: 'runscaler-gate',
    repository_id: 'fixture-repo-a',
    ac: JSON.stringify([{ Scope: 'refs/heads/main', Permission: 3 }]), ...overrides };
  const encoded = [Buffer.from(JSON.stringify({ alg: 'RS256', typ: 'JWT', kid })).toString('base64url'),
    Buffer.from(JSON.stringify(claims)).toString('base64url')].join('.');
  return encoded + '.' + sign('RSA-SHA256', Buffer.from(encoded), signingKey).toString('base64url');
}
async function rpc(method, body, bearer = token()) {
  const headers = { 'content-type': 'application/json' };
  if (bearer !== null) headers.authorization = 'Bearer ' + bearer;
  return fetch(new URL(service + method, base), { method: 'POST', headers,
    body: JSON.stringify(body), redirect: 'error', signal: AbortSignal.timeout(10000) });
}
async function lookup(bearer = token(), requestKey = key, restoreKeys = []) {
  const response = await rpc('GetCacheEntryDownloadURL', { key: requestKey, restore_keys: restoreKeys, version }, bearer);
  assert.equal(response.status, 200, 'cache lookup status');
  return response.json();
}
function localPayloadURL(value) {
  const url = new URL(value);
  assert.equal(url.origin, new URL(base).origin, 'payload must stay on the configured local origin');
  return url;
}
async function assertCancelledUploadsHidden() {
  assert.ok(cancelledKeys.length > 0, 'cancelled upload must have been exercised');
  for (const cancelledKey of cancelledKeys) {
    assert.equal((await lookup(token(), cancelledKey)).ok, false, 'cancelled exact key must miss');
    assert.equal((await lookup(token(), 'missing-cancelled-primary', [cancelledKey])).ok, false,
      'cancelled prefix must miss');
  }
}
async function cancelUpload() {
  const cancelledKey = 'runscaler-cancelled-' + randomUUID();
  const reserved = await rpc('CreateCacheEntry', { key: cancelledKey, version });
  assert.equal(reserved.status, 200, 'reserve cancellation fixture');
  const entry = await reserved.json(); assert.equal(entry.ok, true, 'reserve succeeds');
  const upload = localPayloadURL(entry.signed_upload_url);
  const block = new URL(upload); block.searchParams.set('comp', 'block');
  block.searchParams.set('blockid', Buffer.from(randomUUID() + '000000000000').toString('base64'));
  const staged = await fetch(block, { method: 'PUT', body: payload.subarray(0, 65536), redirect: 'error' });
  assert.equal(staged.status, 201, 'first block staged'); await staged.arrayBuffer();
  block.searchParams.set('blockid', Buffer.from(randomUUID() + '000000000001').toString('base64'));
  // Send part of a declared block, then terminate its socket without end/commit/finalize.
  await new Promise((resolve, reject) => {
    let cancelled = false;
    const request = (block.protocol === 'https:' ? httpsRequest : httpRequest)(block, {
      method: 'PUT', agent: false, headers: { 'content-length': '65536' },
    });
    request.setTimeout(3000, () => request.destroy(new Error('cancellation fixture timed out')));
    request.on('response', response => { response.resume(); reject(new Error('partial block unexpectedly completed')); });
    request.on('error', error => {
      if (cancelled && error.message === 'controlled upload cancellation') resolve(); else reject(error);
    });
    request.write(payload.subarray(0, 4096), error => {
      if (error) { reject(error); return; }
      setTimeout(() => { cancelled = true; request.destroy(new Error('controlled upload cancellation')); }, 100);
    });
  });
  cancelledKeys.push(cancelledKey);
  await assertCancelledUploadsHidden();
  return ['complete block staged', 'next block socket cancelled after partial write',
    'unfinalized exact/prefix restores: miss'];
}
async function run(phase) {
  const started = performance.now();
  if (phase === 'cancellation' || phase === 'cancellation-restore') {
    const results = phase === 'cancellation' ? await cancelUpload() :
      (await assertCancelledUploadsHidden(), ['cancelled exact/prefix restores after recreation: miss']);
    return { schema: 1, phase, ms: Math.round(performance.now() - started), results };
  }
  if (phase === 'roundtrip') key = 'runscaler-controlled-' + randomUUID();
  const results = [];
  const body = { key, version };
  for (const [name, bearer] of [
    ['missing token', null], ['malformed token', 'not-a-jwt'],
    ['wrong issuer', token({ iss: 'http://invalid-issuer.invalid' })],
    ['expired token', token({ exp: 1, nbf: 0 })],
    ['wrong audience', token({ aud: 'other-audience' })],
    ['invalid signature', token({}, generateKeyPairSync('rsa', { modulusLength: 2048 }).privateKey)],
  ]) {
    const response = await rpc('CreateCacheEntry', body, bearer);
    assert.equal(response.status, 401, name + ' must be rejected');
    await response.arrayBuffer();
    results.push(name + ': rejected');
  }
  const readOnly = token({ ac: JSON.stringify([{ Scope: 'refs/heads/main', Permission: 1 }]) });
  const denied = await rpc('CreateCacheEntry', body, readOnly);
  assert.equal(denied.status, 403, 'read-only token cannot save');
  await denied.arrayBuffer();
  results.push('read-only save: rejected');

  if (phase === 'roundtrip') {
    const created = await rpc('CreateCacheEntry', body);
    assert.equal(created.status, 200, 'reserve cache');
    const reservation = await created.json();
    assert.equal(reservation.ok, true, 'reserve cache succeeds');
    const upload = localPayloadURL(reservation.signed_upload_url);
    const blockIDs = [];
    const blockPrefix = randomUUID();
    for (let offset = 0, index = 0; offset < payload.length; offset += 65536, index++) {
      const id = Buffer.from(blockPrefix + String(index).padStart(12, '0')).toString('base64');
      blockIDs.push(id);
      const block = new URL(upload); block.searchParams.set('comp', 'block'); block.searchParams.set('blockid', id);
      const response = await fetch(block, { method: 'PUT', body: payload.subarray(offset, offset + 65536), redirect: 'error' });
      assert.equal(response.status, 201, 'upload block'); await response.arrayBuffer();
    }
    const list = new URL(upload); list.searchParams.set('comp', 'blocklist');
    const committed = await fetch(list, { method: 'PUT', headers: { 'content-type': 'application/xml' },
      body: '<?xml version="1.0" encoding="utf-8"?><BlockList>' + blockIDs.map(id => '<Latest>' + id + '</Latest>').join('') + '</BlockList>', redirect: 'error' });
    assert.equal(committed.status, 201, 'commit block list'); await committed.arrayBuffer();
    const finalized = await rpc('FinalizeCacheEntryUpload', { key, version, size_bytes: String(payload.length) });
    assert.equal(finalized.status, 200, 'finalize cache');
    assert.equal((await finalized.json()).ok, true, 'finalize cache succeeds');
    results.push('authenticated upload: saved');
  } else {
    assert.equal(phase, 'restore', 'supported fixture phase');
  }

  for (const [name, bearer] of [
    ['other repository', token({ repository_id: 'fixture-repo-b' })],
    ['other branch', token({ ac: JSON.stringify([{ Scope: 'refs/heads/other', Permission: 3 }]) })],
  ]) {
    assert.equal((await lookup(bearer)).ok, false, name + ' must not restore');
    results.push(name + ': isolated');
  }
  const restored = await lookup(readOnly);
  assert.equal(restored.ok, true, 'read-only restore');
  const download = localPayloadURL(restored.signed_download_url);
  const response = await fetch(download, { redirect: 'error' });
  assert.equal(response.status, 200, 'download signed cache');
  assert.deepEqual(Buffer.from(await response.arrayBuffer()), payload, 'restored bytes');
  assert.equal((await lookup(token(), 'missing-primary', ['runscaler-controlled-'])).ok, true, 'restore prefix');
  results.push('local restore: checksum verified');
  // Altering a signed download path must reject the capability.
  const malformed = new URL(download); malformed.pathname = '/download/00000000-0000-0000-0000-000000000000';
  const forbidden = await fetch(malformed, { redirect: 'error' });
  assert.equal(forbidden.status, unknownDownloadStatus, 'unknown download capability rejected'); await forbidden.arrayBuffer();
  results.push('unknown download capability: rejected');
  const artifactBefore = artifactRequests;
  const artifact = await fetch(new URL('fixture-artifact', base), { method: 'POST', body: 'synthetic-artifact', redirect: 'error' });
  assert.equal(artifact.status, 200, 'controlled artifact passthrough');
  assert.equal(await artifact.text(), 'synthetic-artifact', 'forwarded artifact bytes');
  assert.equal(artifactRequests, artifactBefore + 1, 'passthrough reaches only the controlled upstream');
  results.push('controlled Results passthrough: verified');
  return { schema: 1, phase, ms: Math.round(performance.now() - started), bytes: payload.length, sha256: createHash('sha256').update(payload).digest('hex'), results };
}

createServer(async (request, response) => {
  try {
    if (request.url === '/jwks') {
      response.setHeader('content-type', 'application/json'); response.end(JSON.stringify({ keys: [jwk] }));
    } else if (request.url === '/buildkit-token' && request.method === 'POST') {
      // Disposable controlled-issuer token, exclusively for the isolated BuildKit fixture.
      response.setHeader('content-type', 'text/plain'); response.end(token());
    } else if (request.url === '/fixture-artifact') {
      artifactRequests++;
      const data = []; for await (const chunk of request) data.push(chunk);
      response.end(Buffer.concat(data));
    } else if (request.url === '/run' && request.method === 'POST') {
      if (busy) { response.writeHead(409); response.end('fixture already running'); return; }
      busy = true;
      try {
        const chunks = []; for await (const chunk of request) chunks.push(chunk);
        const { phase = 'roundtrip' } = JSON.parse(Buffer.concat(chunks).toString() || '{}');
        const result = await run(phase);
        response.setHeader('content-type', 'application/json'); response.end(JSON.stringify(result));
      } finally { busy = false; }
    } else { response.writeHead(404); response.end('not found'); }
  } catch (error) {
    response.writeHead(500, { 'content-type': 'application/json' });
    response.end(JSON.stringify({ error: error.message }));
  }
}).listen(Number(new URL(issuer).port || 4080), issuerBindHost, () => console.log('controlled issuer fixture ready'));
