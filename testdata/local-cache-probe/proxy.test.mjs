import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
test('measurement proxy verifies payload routes and counts without retaining capabilities', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cache-proxy-'));
  const secret = 'secret-capability-never-record';
  let outside = 0;
  const foreign = http.createServer((req,res) => { outside++; res.end('{}'); });
  const reserve = http.createServer();
  await new Promise(r => reserve.listen(0, '127.0.0.1', r));
  const port = reserve.address().port; await new Promise(r => reserve.close(r));
  const advertised = `http://127.0.0.1:${port}`;
  await new Promise(r => foreign.listen(0, '127.0.0.1', r));
  const foreignURL = `http://127.0.0.1:${foreign.address().port}`;
  const backend = http.createServer((req,res) => {
    if (req.url === '/payload-redirect') { res.writeHead(302, { location: foreignURL + '/cache-payload' }).end(); return; }
    if (req.url.includes('.CacheService/')) {
      req.resume(); req.on('end', () => res.end(JSON.stringify({ signed_download_url: req.url.endsWith('redirectmeta') ? advertised + '/payload-redirect' : (req.url.endsWith('foreign') ? foreignURL : advertised) + '/' + secret })));
    } else if (req.method === 'PUT') { req.resume(); req.on('end', () => res.end()); }
    else res.end('verified-payload');
  });
  await new Promise(r => backend.listen(0, '127.0.0.1', r));
  const child = spawn(process.execPath, [new URL('proxy.mjs', import.meta.url).pathname], { env: { ...process.env,
    PROBE_PROXY_PORT: String(port), PROBE_UPSTREAM_URL: `http://127.0.0.1:${backend.address().port}`,
    PROBE_ADVERTISE_URL: advertised, PROBE_PROXY_LOG: path.join(dir, 'metrics') }, stdio: ['ignore','pipe','pipe'] });
  try {
    await once(child.stdout, 'data');
    const rpc = '/twirp/github.actions.results.api.v1.CacheService/GetCacheEntryDownloadURL';
    const meta = await (await fetch(advertised + rpc, { method: 'POST', body: JSON.stringify({ key: 'index-runscaler-linux-amd64-fixture-v1-1-refs/heads/main' }) })).json();
    assert.equal(await (await fetch(meta.signed_download_url)).text(), 'verified-payload');
    await fetch(meta.signed_download_url, { method: 'PUT', body: '1234567' });
    const rust = await (await fetch(advertised + rpc, { method: 'POST', body: JSON.stringify({ key: 'v0-rust-runscaler-linux-amd64-fixture-v1-Linux-x64-hash' }) })).json();
    await (await fetch(rust.signed_download_url)).text();
    const redirected = await (await fetch(advertised + rpc + '/redirectmeta', { method: 'POST', body: '{}' })).json();
    assert.equal((await fetch(redirected.signed_download_url)).status, 502);
    assert.equal((await fetch(advertised + rpc + '/foreign', { method: 'POST', body: '{}' })).status, 502);
    const status = await new Promise((resolve,reject) => http.request(advertised, { path: foreignURL + rpc }, res => { res.resume(); res.on('end', () => resolve(res.statusCode)); }).on('error',reject).end());
    assert.equal(status, 403);
    assert.equal(outside, 0);
    await new Promise(r=>setTimeout(r,20));
    const log = fs.readFileSync(path.join(dir,'metrics'),'utf8');
    const rows = log.trim().split('\n').map(JSON.parse);
    assert.equal(rows.filter(x=>x.kind==='download').reduce((n,x)=>n+x.bytes,0), Buffer.byteLength('verified-payload') * 2);
    assert.equal(rows.filter(x=>x.kind==='download' && x.workload==='docker').length, 1);
    assert.equal(rows.filter(x=>x.kind==='upload').reduce((n,x)=>n+x.bytes,0), 7);
    assert.ok(!log.includes(secret));
  } finally { child.kill(); await once(child,'exit'); backend.close(); foreign.close(); fs.rmSync(dir,{recursive:true}); }
});
