const test = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const { spawn } = require('node:child_process');
const { mkdtempSync, readFileSync, rmSync } = require('node:fs');
const { tmpdir } = require('node:os');
const path = require('node:path');

test('observer measures local payload bytes and rejects foreign cache RPC without logging capabilities', async () => {
  const secret = 'opaque-capability-DO-NOT-LOG';
  let foreignRequests = 0;
  const foreign = http.createServer((req, res) => { foreignRequests++; res.end('{}'); });
  const local = http.createServer((req, res) => {
    if (req.url.endsWith('/fail')) { req.socket.destroy(); return; }
    if (req.url === '/payload-redirect') { res.writeHead(302, { location: other + '/cache-payload' }).end(); return; }
    if (req.url.includes('CacheService/')) res.end(JSON.stringify({ signed_download_url: req.url.endsWith('/redirectmeta') ? base + '/payload-redirect' : `${base}/${secret}` }));
    else if (req.method === 'PUT') { req.resume(); req.on('end', () => res.end()); }
    else res.end('verified-cache-payload');
  });
  await Promise.all([new Promise(r => local.listen(0, '127.0.0.1', r)), new Promise(r => foreign.listen(0, '127.0.0.1', r))]);
  const base = `http://127.0.0.1:${local.address().port}`;
  const other = `http://127.0.0.1:${foreign.address().port}`;
  const dir = mkdtempSync(path.join(tmpdir(), 'cache-observer-'));
  try {
    const code = `const http=require('node:http');
      (async()=>{
        const rpc='/twirp/github.actions.results.api.v1.CacheService/GetCacheEntryDownloadURL';
        const meta=await (await fetch(${JSON.stringify(base)}+rpc)).json();
        await (await fetch(meta.signed_download_url)).arrayBuffer();
        await new Promise((resolve,reject)=>{const req=http.request(meta.signed_download_url,{method:'PUT'},res=>{res.resume();res.on('end',resolve)});req.on('error',reject);req.write('abc');req.end('de')});
        await fetch(meta.signed_download_url,{method:'PUT',body:'1234567'});
        await new Promise((resolve,reject)=>http.get(${JSON.stringify(base)}+rpc,res=>{let text='';res.on('data',x=>text+=x);res.on('end',()=>http.get(JSON.parse(text).signed_download_url,res=>{res.resume();res.on('end',resolve)}).on('error',reject))}).on('error',reject));
        const redirected=await (await fetch(${JSON.stringify(base)}+rpc+'/redirectmeta')).json();
        await new Promise(resolve=>http.get(redirected.signed_download_url,res=>{if(res.headers.location)http.get(res.headers.location,r=>{r.resume();r.on('end',()=>{process.exitCode=8;resolve()})}).on('error',resolve);else{res.resume();res.on('end',resolve);res.on('error',resolve)}}).on('error',resolve));
        try { await fetch(${JSON.stringify(base)}+rpc+'/fail'); } catch {}
        await new Promise(resolve=>http.get(${JSON.stringify(base)}+rpc+'/fail').on('error',resolve));
        try { await fetch(${JSON.stringify(other)}+rpc); process.exitCode=4; } catch {}
        try { http.get(${JSON.stringify(other)}+rpc); process.exitCode=5; } catch {}
        await (await fetch(${JSON.stringify(other)}+'/artifact')).text();
      })().catch(()=>{process.exitCode=6});`;
    const result = await new Promise(resolve => {
      const child = spawn(process.execPath, ['--require', path.join(__dirname, 'observer.cjs'), '-e', code], {
        env: { ...process.env, EXPECTED_CACHE_URL: base + '/', CACHE_OBSERVER_FILE: path.join(dir, 'metrics.jsonl') }, stdio: ['ignore', 'pipe', 'pipe']
      });
      let output = ''; child.stdout.on('data', x => output += x); child.stderr.on('data', x => output += x);
      child.on('exit', code => resolve({ code, output }));
    });
    assert.equal(result.code, 0, result.output);
    assert.equal(foreignRequests, 1, 'only ordinary artifact request may leave the cache origin');
    const text = readFileSync(path.join(dir, 'metrics.jsonl'), 'utf8');
    const rows = text.trim().split('\n').map(JSON.parse);
    assert.equal(rows.filter(x => x.kind === 'download').reduce((sum, x) => sum + x.bytes, 0), Buffer.byteLength('verified-cache-payload') * 2);
    assert.equal(rows.filter(x => x.kind === 'blocked').length, 3);
    assert.equal(rows.filter(x => x.kind === 'rpc-error').length, 2);
    assert.equal(rows.filter(x => x.kind === 'upload').reduce((sum, x) => sum + x.bytes, 0), 12);
    assert.ok(rows.every(x => x.schema === 1 && !('url' in x)));
    assert.ok(!text.includes(secret));
  } finally { local.close(); foreign.close(); rmSync(dir, { recursive: true }); }
});
