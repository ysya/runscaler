// Test-only preload. Never log headers, tokens, keys, payload paths or capabilities.
const fs = require('node:fs');
const http = require('node:http');
const https = require('node:https');
const childProcess = require('node:child_process');
const expected = new URL(process.env.EXPECTED_CACHE_URL);
const output = process.env.CACHE_OBSERVER_FILE;
const payloads = new Set();
function record(kind, started, bytes = 0, status = 0) {
  fs.appendFileSync(output, JSON.stringify({ schema: 1, kind, origin: expected.origin,
    ms: Math.round(performance.now() - started), bytes, status }) + '\n');
}
function classify(url) {
  const rpc = url.pathname.includes('.CacheService/') || url.pathname.includes('/_apis/artifactcache');
  if (rpc && url.origin !== expected.origin) { record('blocked', performance.now()); throw new Error('Foreign cache endpoint refused'); }
  return rpc ? 'rpc' : payloads.has(url.origin + url.pathname) ? 'payload' : null;
}
function metadata(text) {
  let value; try { value = JSON.parse(text); } catch { return; }
  for (const key of ['signed_upload_url', 'signed_download_url', 'signedUploadUrl', 'signedDownloadUrl']) {
    if (!value[key]) continue;
    const url = new URL(value[key]);
    if (url.origin !== expected.origin) { record('blocked', performance.now()); throw new Error('Foreign cache payload refused'); }
    payloads.add(url.origin + url.pathname);
    if (process.env.GITHUB_ACTIONS === 'true') process.stdout.write('::add-mask::' + value[key] + '\n');
  }
}
function requestURL(protocol, args) {
  const first = args[0];
  if (typeof first === 'string' || first instanceof URL) return new URL(first);
  return new URL(`${first.protocol || protocol}//${first.hostname || first.host}${first.port ? ':' + first.port : ''}${first.path || '/'}`);
}
for (const [module, protocol] of [[http, 'http:'], [https, 'https:']]) {
  const original = module.request;
  module.request = function (...args) {
    const kind = classify(requestURL(protocol, args));
    const start = performance.now();
    const req = original.apply(this, args);
    if (!kind) return req;
    req.once('error', () => record(kind === 'rpc' ? 'rpc-error' : 'payload-error', start));
    let sent = 0;
    const write = req.write;
    req.write = function (chunk, ...rest) { if (chunk) sent += Buffer.byteLength(chunk); return write.call(this, chunk, ...rest); };
    const end = req.end;
    req.end = function (chunk, ...rest) { if (typeof chunk === 'string' || Buffer.isBuffer(chunk)) sent += Buffer.byteLength(chunk); return end.call(this, chunk, ...rest); };
    req.prependListener('response', res => {
      if (kind === 'payload' && res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
        const target = new URL(res.headers.location, expected);
        if (target.origin !== expected.origin) {
          record('blocked', start); delete res.headers.location;
          req.destroy(new Error('Foreign cache redirect refused')); res.destroy(); return;
        }
        payloads.add(target.origin + target.pathname);
      }
      let received = 0; const chunks = []; const push = res.push;
      res.push = function (chunk, ...rest) {
        if (chunk) { received += chunk.length; if (kind === 'rpc' && received <= 1048576) chunks.push(Buffer.from(chunk)); }
        else {
          if (kind === 'rpc') { try { metadata(Buffer.concat(chunks).toString()); } catch (error) { res.destroy(error); return false; } }
          record(kind === 'rpc' ? 'rpc' : req.method === 'PUT' ? 'upload' : 'download', start, kind === 'rpc' ? 0 : req.method === 'PUT' ? sent : received, res.statusCode);
        }
        return push.call(this, chunk, ...rest);
      };
    });
    return req;
  };
  // Native get closes its original request implementation; route it through ours.
  module.get = function (...args) { const req = module.request(...args); req.end(); return req; };
}
const fetch = globalThis.fetch;
globalThis.fetch = async function (input, init) {
  const url = new URL(typeof input === 'string' || input instanceof URL ? input : input.url);
  const kind = classify(url); const start = performance.now();
  let response;
  try { response = await fetch(input, kind ? { ...init, redirect: 'error' } : init); }
  catch (error) { if (kind) record(kind === 'rpc' ? 'rpc-error' : 'payload-error', start); throw error; }
  if (kind === 'rpc') { metadata(await response.clone().text()); record('rpc', start, 0, response.status); }
  else if (kind === 'payload') {
    const method = init?.method || input.method || 'GET';
    if (method === 'PUT') record('upload', start, init?.body ? Buffer.byteLength(init.body) : 0, response.status);
    else { const bytes = (await response.clone().arrayBuffer()).byteLength; record('download', start, bytes, response.status); }
  }
  return response;
};
const spawn = childProcess.spawn;
childProcess.spawn = function (file, args, ...rest) {
  const start = performance.now(); const child = spawn.call(this, file, args, ...rest);
  if (String(file).endsWith('/tar') || file === 'tar') {
    const archive = (args || []).some(x => /cache\.(t?gz|tzst|tar)/.test(x));
    if (archive) child.once('exit', code => record(args.some(x => x === '-xf' || x === '-x') ? 'extract' : 'archive', start, 0, code));
  }
  return child;
};
