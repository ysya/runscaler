// Isolated measurement proxy for non-Node clients (not a production service).
import http from 'node:http';
import fs from 'node:fs';
const base = new URL(process.env.PROBE_UPSTREAM_URL);
const advertised = new URL(process.env.PROBE_ADVERTISE_URL);
const payloads = new Map();
const log = process.env.PROBE_PROXY_LOG;
function record(kind, workload, bytes, start, status) {
  fs.appendFileSync(log, JSON.stringify({ schema: 1, kind, workload, bytes,
    ms: Math.round(performance.now() - start), status, origin: advertised.origin }) + '\n');
}
http.createServer(async (incoming, outgoing) => {
  const start = performance.now();
  const location = new URL(incoming.url, advertised);
  if (location.origin !== advertised.origin) { outgoing.writeHead(403).end(); record('blocked', 'unknown', 0, start, 403); return; }
  const rpc = location.pathname.includes('.CacheService/');
  let workload = payloads.get(location.pathname) || 'other';
  let body;
  if (rpc) {
    const chunks = []; let size = 0;
    for await (const chunk of incoming) { size += chunk.length; if (size > 1048576) { outgoing.writeHead(413).end(); return; } chunks.push(chunk); }
    body = Buffer.concat(chunks);
    try { const key = JSON.parse(body).key || ''; workload = (key.startsWith('index-runscaler-linux-amd64-fixture') || key.startsWith('buildkit-blob-')) ? 'docker' : 'toolkit'; } catch { workload = 'unknown'; }
  }
  const upstream = http.request(new URL(incoming.url, base), { method: incoming.method, headers: incoming.headers }, response => {
    if (workload !== 'other' && !rpc && response.statusCode >= 300 && response.statusCode < 400 && response.headers.location) {
      const target = new URL(response.headers.location, location);
      if (target.origin !== advertised.origin) {
        response.resume(); outgoing.writeHead(502).end(); record('blocked', workload, 0, start, 502); return;
      }
      payloads.set(target.pathname, workload);
    }
    let received = 0; const chunks = [];
    response.on('data', chunk => { received += chunk.length; if (rpc && received <= 1048576) chunks.push(chunk); });
    if (rpc) {
      response.on('end', () => {
        const data = Buffer.concat(chunks);
        try {
          const metadata = JSON.parse(data);
          for (const name of ['signed_upload_url', 'signed_download_url', 'signedUploadUrl', 'signedDownloadUrl']) {
            if (!metadata[name]) continue;
            const target = new URL(metadata[name]);
            if (target.origin !== advertised.origin) throw new Error('foreign payload');
            payloads.set(target.pathname, workload);
          }
        } catch (error) { if (error.message === 'foreign payload') { outgoing.writeHead(502).end(); record('blocked', workload, 0, start, 502); return; } }
        outgoing.writeHead(response.statusCode, response.headers).end(data);
        record('rpc', workload, 0, start, response.statusCode);
      });
    } else {
      outgoing.writeHead(response.statusCode, response.headers); response.pipe(outgoing);
      response.on('end', () => { if (incoming.method === 'GET' && workload !== 'other') record('download', workload, received, start, response.statusCode); });
    }
  });
  let sent = body?.length || 0;
  if (!rpc) incoming.on('data', chunk => { sent += chunk.length; });
  upstream.on('response', response => { if (incoming.method === 'PUT' && workload !== 'other') response.once('end', () => record('upload', workload, sent, start, response.statusCode)); });
  upstream.on('error', () => { if (!outgoing.headersSent) outgoing.writeHead(502); outgoing.end(); record('upstream-error', workload, 0, start, 502); });
  incoming.on('aborted', () => upstream.destroy());
  if (body) upstream.end(body); else incoming.pipe(upstream);
}).listen(Number(process.env.PROBE_PROXY_PORT || 3000), '0.0.0.0', () => console.log('isolated measurement proxy ready'));
