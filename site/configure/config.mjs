import { quoteToml } from './toml.mjs';
import { groups } from './schema.mjs';

const copy = value => String(value ?? '').trim();
const list = value => String(value ?? '').split(/\r?\n/u).map(item => item.trim()).filter(Boolean);
const fieldName = (field, lang) => lang === 'zh-TW' ? field.zh : field.en;
const message = (lang, en, zh) => lang === 'zh-TW' ? zh : en;
const fieldError = (group, key, index, text) => Object.assign(new Error(text), { field: { group, key, index } });
const reject = (condition, group, key, index, lang, en, zh) => {
  if (condition) throw fieldError(group, key, index, message(lang, en, zh));
};
const volumeName = /^[a-zA-Z0-9][a-zA-Z0-9_.-]*$/u;
function quoteField(value, group, key, index) {
  try { return quoteToml(value); }
  catch (error) { throw fieldError(group, key, index, error.message); }
}

function cleanPath(value) {
  return value.startsWith('/') && value !== '/' && !value.endsWith('/') && !value.split('/').slice(1).some(part => !part || part === '.' || part === '..');
}

function threshold(value) {
  if (value.endsWith('%')) {
    const number = value.slice(0, -1);
    if (!/^[+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:e[+-]?\d+)?$/iu.test(number)) return null;
    const percent = Number(number);
    return percent >= 0 && percent <= 100 ? { type: 'percent', value: percent } : null;
  }
  const match = /^(\d+)(B|KB|MB|GB|TB)$/iu.exec(value);
  if (!match) return null;
  const exponent = ['B', 'KB', 'MB', 'GB', 'TB'].indexOf(match[2].toUpperCase());
  const bytes = BigInt(match[1]) * 1024n ** BigInt(exponent);
  return bytes <= 18446744073709551615n ? { type: 'bytes', value: bytes } : null;
}

function durationNs(value) {
  if (value === '0') return 0;
  const parts = [...value.matchAll(/(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/gu)];
  if (!parts.length || parts.map(part => part[0]).join('') !== value.replace(/^-/, '')) return NaN;
  const units = { ns: 1, us: 1e3, 'µs': 1e3, ms: 1e6, s: 1e9, m: 60e9, h: 3600e9 };
  return (value.startsWith('-') ? -1 : 1) * parts.reduce((sum, part) => sum + Number(part[1]) * units[part[2]], 0);
}

function validIP(value) {
  if (value.includes(':')) {
    if (value.includes('[') || value.includes(']') || value.includes('%')) return false;
    try { new URL(`http://[${value}]/`); return true; } catch { return false; }
  }
  const parts = value.split('.');
  return parts.length === 4 && parts.every(part => /^(?:0|[1-9]\d{0,2})$/u.test(part) && Number(part) <= 255);
}

function privateListen(value, healthPort) {
  const match = /^(?:\[([^\]]+)\]|([^:]+)):(\d+)$/u.exec(value);
  if (!match) return false;
  const port = Number(match[3]);
  if (!Number.isInteger(port) || port < 1 || port > 65535 || port === Number(healthPort)) return false;
  if (match[1]) {
    return validIP(match[1]) && /^(?:fc|fd)[0-9a-f:]+$/iu.test(match[1]);
  }
  if (!validIP(match[2])) return false;
  const parts = match[2].split('.');
  const [a, b] = parts.map(Number);
  return a === 10 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168);
}

function validURL(value, schemes, rootOnly = false) {
  let parsed;
  try { parsed = new URL(value); } catch { return false; }
  return schemes.includes(parsed.protocol) && Boolean(parsed.hostname) && !parsed.username && !parsed.password && !value.includes('?') && !value.includes('#') && (!rootOnly || parsed.pathname === '/') && (!rootOnly || value.endsWith('/'));
}

function serialize(field, raw, lang) {
  if (raw === undefined || raw === null || String(raw).trim() === '') return null;
  const value = copy(raw);
  if (field.type === 'number') {
    const number = Number(value);
    if (!Number.isSafeInteger(number) || (field.min !== undefined && number < field.min) || (field.max !== undefined && number > field.max)) {
      throw new Error(message(lang, `Invalid number: ${fieldName(field, lang)}`, `數值無效：${fieldName(field, lang)}`));
    }
    return String(number);
  }
  if (field.type === 'bool') {
    if (value !== 'true' && value !== 'false') throw new Error(`Invalid boolean: ${field.key}`);
    return value;
  }
  if (field.type === 'select' && !field.choices.includes(value)) throw new Error(`Invalid choice: ${field.key}`);
  if (field.type === 'lines') return `[${list(raw).map(quoteToml).join(', ')}]`;
  const httpsOnly = ['url', 'token-issuer', 'jwks-url', 'upstream-results-url'].includes(field.key);
  if (field.type === 'url' && !validURL(value, httpsOnly ? ['https:'] : ['https:', 'http:'])) {
    throw new Error(message(lang, `Invalid URL: ${fieldName(field, lang)}`, `網址無效：${fieldName(field, lang)}`));
  }
  if (field.type === 'duration' && !/^(?:0|-?(?:\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h))+)$/u.test(value)) {
    throw new Error(message(lang, `Use a Go duration such as 5m or 24h: ${fieldName(field, lang)}`, `請輸入 Go 時間格式，例如 5m 或 24h：${fieldName(field, lang)}`));
  }
  return quoteToml(value);
}

function emitFields(values = {}, fields, lang, group, index = 0, excluded = []) {
  return fields.filter(field => !excluded.includes(field.key) && field.type !== 'cache').flatMap(field => {
    let encoded;
    try { encoded = serialize(field, values[field.key], lang); }
    catch (error) { throw fieldError(group, field.key, index, error.message); }
    return encoded === null ? [] : [`${field.key} = ${encoded}`];
  });
}

function emitTable(header, values, fields, lang, group, index = 0) {
  const lines = emitFields(values, fields, lang, group, index);
  return lines.length ? ['', `[${header}]`, ...lines] : [];
}

function legacyEntries(raw) {
  return list(raw).map((entry, index) => {
    const [name, path, budget = '', onExceed = '', ...rest] = entry.split('|').map(part => part.trim());
    return { name, path, budget, onExceed, rest, line: index + 1 };
  });
}

function validateLegacyCache(docker, index, lang) {
  const short = list(docker['cache-volumes']);
  const long = legacyEntries(docker.cache);
  const byName = new Map();
  const shortNames = new Set();
  const longNames = new Set();
  const invalid = (key, line) => { throw fieldError('docker', key, index, message(lang, `Invalid legacy cache volume on line ${line}. Use a unique name and clean absolute path.`, `第 ${line} 行的舊版 cache volume 無效；請使用不重複的名稱及乾淨的絕對路徑。`)); };
  short.forEach((entry, line) => {
    const colon = entry.indexOf(':');
    const name = entry.slice(0, colon);
    const path = entry.slice(colon + 1);
    if (colon < 0 || !volumeName.test(name) || !cleanPath(path) || shortNames.has(name)) invalid('cache-volumes', line + 1);
    shortNames.add(name);
    byName.set(name, { path, key: 'cache-volumes', line: line + 1 });
  });
  long.forEach(entry => {
    if (entry.rest.length || !volumeName.test(entry.name) || !cleanPath(entry.path) || (entry.budget && (!threshold(entry.budget) || threshold(entry.budget).type !== 'bytes')) || (entry.onExceed && !['warn', 'wipe'].includes(entry.onExceed)) || longNames.has(entry.name)) invalid('cache', entry.line);
    longNames.add(entry.name);
    byName.set(entry.name, { path: entry.path, key: 'cache', line: entry.line });
  });
  const paths = new Set();
  for (const entry of byName.values()) {
    if (paths.has(entry.path)) invalid(entry.key, entry.line);
    paths.add(entry.path);
  }
  return paths;
}

function emitLegacyCache(header, raw, lang, index) {
  return legacyEntries(raw).flatMap(entry => {
    try {
      const lines = ['', `[[${header}]]`, `name = ${quoteToml(entry.name)}`, `path = ${quoteToml(entry.path)}`];
      if (entry.budget) lines.push(`budget = ${quoteToml(entry.budget)}`);
      if (entry.onExceed) lines.push(`on-exceed = ${quoteToml(entry.onExceed)}`);
      return lines;
    } catch (error) { throw fieldError('docker', 'cache', index, error.message); }
  });
}

function validateSet(scale, global, lang, index) {
  const set = scale.set ?? {};
  const provider = copy(set.provider) || 'docker';
  const auth = copy(set.auth) || 'pat';
  reject(!validURL(copy(set.url), ['https:']) || new URL(set.url || 'https://github.com/').pathname === '/', 'set', 'url', index, lang, 'Enter an HTTPS GitHub organization or repository URL.', '請輸入 HTTPS GitHub 組織或儲存庫網址。');
  reject(!copy(set.name), 'set', 'name', index, lang, 'Enter a scale-set name.', '請輸入規模集名稱。');
  reject(provider !== 'docker' && provider !== 'tart', 'set', 'provider', index, lang, 'Invalid provider.', '執行環境無效。');
  reject(auth !== 'pat' && auth !== 'app', 'set', 'auth', index, lang, 'Invalid authentication.', '驗證方式無效。');
  const max = Number(set['max-runners']);
  const min = copy(set['min-runners']) === '' ? 0 : Number(set['min-runners']);
  reject(!Number.isSafeInteger(max) || max < 1, 'set', 'max-runners', index, lang, 'Maximum runners must be a positive whole number.', '最多 runner 數量須為正整數。');
  reject(!Number.isSafeInteger(min) || min < 0 || min > max, 'set', 'min-runners', index, lang, 'Runner limits must be whole numbers with 0 ≤ minimum ≤ maximum.', 'Runner 數量須為整數，且 0 ≤ 下限 ≤ 上限。');
  reject(provider === 'tart' && max > 2, 'set', 'max-runners', index, lang, 'Tart allows at most two concurrent VMs per host.', 'Tart 每台主機最多同時執行兩台 VM。');
  reject(!copy(set['runner-image']), 'set', 'runner-image', index, lang, 'Enter a runner image.', '請輸入 runner 映像檔。');
  if (auth === 'app') {
    let path;
    try { path = decodeURIComponent(new URL(set.url).pathname); } catch { throw fieldError('set', 'url', index, message(lang, 'Invalid GitHub URL.', 'GitHub 網址無效。')); }
    reject(path.replace(/^\/+/, '').startsWith('enterprises/'), 'set', 'auth', index, lang, 'Use a PAT for enterprise-level runners.', '企業層級 runner 請使用 PAT。');
    reject(!copy(set['client-id']), 'set', 'client-id', index, lang, 'Enter the GitHub App client ID.', '請填寫 GitHub App 用戶端 ID。');
    reject(!Number.isSafeInteger(Number(set['installation-id'])) || Number(set['installation-id']) < 1, 'set', 'installation-id', index, lang, 'Enter a positive installation ID.', '請填寫正整數安裝 ID。');
    if (set['app-key-source'] === 'env') reject(!/^[A-Za-z_][A-Za-z0-9_]*$/u.test(copy(set['key-env'])), 'set', 'key-env', index, lang, 'Enter a valid environment variable name.', '請輸入有效的環境變數名稱。');
    else reject(!copy(set['key-file']).startsWith('/'), 'set', 'key-file', index, lang, 'Enter an absolute key-file path.', '請輸入私鑰檔案絕對路徑。');
  } else if (copy(set['token-env']) && (!/^[A-Za-z_][A-Za-z0-9_]*$/u.test(copy(set['token-env'])) || /^(?:ghp_|gho_|ghu_|ghs_|ghr_|github_pat_)/iu.test(copy(set['token-env'])))) {
    throw fieldError('set', 'token-env', index, message(lang, 'Enter a valid PAT environment variable name.', '請輸入有效的 PAT 環境變數名稱。'));
  }
  const retry = scale.retry ?? {};
  const initial = durationNs(copy(retry.initial) || '5s');
  const retryMax = durationNs(copy(retry.max) || '1m');
  const cooldown = durationNs(copy(retry.cooldown) || '5m');
  reject(!(initial > 0), 'retry', 'initial', index, lang, 'Initial retry delay must be positive.', '初次重試間隔須為正數。');
  reject(!(retryMax >= initial), 'retry', 'max', index, lang, 'Maximum retry delay must be at least the initial delay.', '最大重試間隔不可小於初次重試間隔。');
  reject(!(cooldown >= retryMax), 'retry', 'cooldown', index, lang, 'Cooldown must be at least the maximum retry delay.', '冷卻時間不可小於最大重試間隔。');
  reject(copy(retry['cooldown-after']) && !(Number(retry['cooldown-after']) >= 1), 'retry', 'cooldown-after', index, lang, 'Cooldown failure count must be positive.', '進入冷卻前的失敗次數須為正數。');
  if (provider === 'docker') {
    const docker = scale.docker ?? {};
    reject(copy(docker['ready-timeout']) && !(durationNs(copy(docker['ready-timeout'])) > 0), 'docker', 'ready-timeout', index, lang, 'Readiness timeout must be positive.', '就緒逾時須為正數。');
    reject(copy(docker['workspace-root']) && copy(docker.dind) === 'false', 'docker', 'workspace-root', index, lang, 'Docker workspace root requires Docker socket access.', 'Docker 工作目錄需要開啟 Docker socket 存取。');
    reject(copy(docker['workspace-root']) && !cleanPath(copy(docker['workspace-root'])), 'docker', 'workspace-root', index, lang, 'Workspace root must be a clean absolute path other than /.', '工作目錄須為乾淨的絕對路徑，且不可為 /。');
    reject(copy(docker.platform) && !/^[^/]+\/[^/]+(?:\/[^/]+)?$/u.test(copy(docker.platform)), 'docker', 'platform', index, lang, 'Docker platform must be os/arch or os/arch/variant.', 'Docker 平台須為 os/arch 或 os/arch/variant。');
    reject(copy(docker['shared-volume-name']) && !volumeName.test(copy(docker['shared-volume-name'])), 'docker', 'shared-volume-name', index, lang, 'Invalid shared volume name.', '共用 volume 名稱無效。');
    reject(copy(docker['shared-volume']) && !cleanPath(copy(docker['shared-volume'])), 'docker', 'shared-volume', index, lang, 'Shared volume must be a clean absolute path other than /.', '共用 volume 須為乾淨的絕對路徑，且不可為 /。');
    const cachePaths = validateLegacyCache(docker, index, lang);
    reject(cachePaths.has(copy(docker['shared-volume'])), 'docker', 'shared-volume', index, lang, 'Shared volume conflicts with a legacy cache mount path.', '共用 volume 與舊版快取掛載路徑衝突。');
    reject(copy(docker['native-actions-cache']) === 'true' && copy(global['actions-cache']?.enabled) !== 'true', 'docker', 'native-actions-cache', index, lang, 'Enable the global native Actions cache receiver first.', '請先啟用全域內建 Actions 快取接收端。');
    reject(copy(docker['actions-cache-url']) && (!validURL(copy(docker['actions-cache-url']), ['http:', 'https:']) || !copy(docker['actions-cache-url']).endsWith('/')), 'docker', 'actions-cache-url', index, lang, 'Actions cache URL must end with / and contain no credentials, query, or fragment.', 'Actions 快取網址須以 / 結尾，且不可包含憑證、查詢參數或片段。');
    reject(copy(docker['native-actions-cache']) === 'true' && copy(docker['actions-cache-url']) && docker['actions-cache-url'] !== global['actions-cache']?.['advertise-url'], 'docker', 'actions-cache-url', index, lang, 'Native and explicit cache URLs must match.', '內建與明確指定的快取網址必須相同。');
  }
  if (provider === 'tart') {
    const tart = scale.tart ?? {};
    reject(copy(tart['pool-size']) && Number(tart['pool-size']) > max, 'tart', 'pool-size', index, lang, 'Tart warm pool cannot exceed maximum runners.', 'Tart 預熱 VM 數量不可超過最多 runner 數量。');
    reject(copy(tart.mtu) && ![0, -1].includes(Number(tart.mtu)) && (Number(tart.mtu) < 1280 || Number(tart.mtu) > 1500), 'tart', 'mtu', index, lang, 'Tart MTU must be 0, -1, or 1280–1500.', 'Tart MTU 須為 0、-1 或 1280–1500。');
    reject(list(tart.dns).some(ip => !validIP(ip)), 'tart', 'dns', index, lang, 'Tart DNS entries must be IP addresses.', 'Tart DNS 項目須為 IP 位址。');
    reject(copy(tart['net-bridged']) && (/^-/u.test(copy(tart['net-bridged'])) || /[\s=]/u.test(copy(tart['net-bridged']))), 'tart', 'net-bridged', index, lang, 'Enter a valid host interface name.', '請輸入有效的主機網路介面名稱。');
  }
}

export function generateFullConfig(state, lang = 'en') {
  const scales = state.scaleSets ?? [];
  if (!scales.length) throw new Error(message(lang, 'Add a scale set.', '請新增規模集。'));
  const global = { ...state.global, 'actions-cache': state.actionsCache };
  scales.forEach((scale, index) => validateSet(scale, global, lang, index));
  let tartMinimum = 0;
  let tartPool = 0;
  scales.forEach((scale, index) => {
    if (scale.set.provider !== 'tart') return;
    tartMinimum += Number(scale.set['min-runners'] || 0);
    reject(tartMinimum > 2, 'set', 'min-runners', index, lang, 'Combined Tart warm runners cannot exceed two VMs per host.', 'Tart 常駐 runner 總數不可超過每台主機兩台 VM。');
    tartPool += Number(scale.tart?.['pool-size'] || 0);
    reject(tartPool > 2, 'tart', 'pool-size', index, lang, 'Combined Tart warm pool cannot exceed two VMs per host.', 'Tart 預熱 VM 總數不可超過每台主機兩台 VM。');
  });
  const names = scales.map(scale => scale.set.name.trim());
  const duplicate = names.findIndex((name, index) => names.indexOf(name) !== index);
  reject(duplicate !== -1, 'set', 'name', duplicate, lang, 'Scale-set names must be unique.', '規模集名稱不可重複。');
  const cache = state.actionsCache ?? {};
  if (copy(cache.enabled) === 'true') {
    reject(!privateListen(copy(cache['listen-address']), state.global?.['health-port'] ?? '8080'), 'actionsCache', 'listen-address', 0, lang, 'Use a private IP and port different from the health port.', '請使用私人 IP 與連接埠，且不可與健康檢查連接埠相同。');
    reject(!validURL(copy(cache['advertise-url']), ['http:', 'https:'], true), 'actionsCache', 'advertise-url', 0, lang, 'Advertise URL must be a root HTTP(S) URL ending with /.', '公開網址須為以 / 結尾的 HTTP(S) 根路徑網址。');
    reject(!cleanPath(copy(cache['data-dir'])), 'actionsCache', 'data-dir', 0, lang, 'Data directory must be a clean absolute private path other than /.', '資料目錄須為乾淨的私人絕對路徑，且不可為 /。');
    reject(!(Number(cache['max-size-bytes']) > 0), 'actionsCache', 'max-size-bytes', 0, lang, 'Cache byte limit must be positive.', '快取位元組上限須為正數。');
    reject(copy(cache['token-issuer']) && copy(cache['token-issuer']) !== 'https://token.actions.githubusercontent.com' && !copy(cache['jwks-url']), 'actionsCache', 'jwks-url', 0, lang, 'A custom token issuer requires a JWKS URL.', '自訂權杖簽發者需要 JWKS 網址。');
  }
  const disk = state.disk ?? {};
  const minFree = threshold(copy(disk['min-free']) || '10%');
  const targetFree = threshold(copy(disk['target-free']) || '20%');
  reject(!minFree, 'disk', 'min-free', 0, lang, 'Use a percentage (10%) or size (20GB).', '請輸入百分比（10%）或容量（20GB）。');
  reject(!targetFree, 'disk', 'target-free', 0, lang, 'Use a percentage (20%) or size (40GB).', '請輸入百分比（20%）或容量（40GB）。');
  reject(minFree.type !== targetFree.type, 'disk', 'target-free', 0, lang, 'Both disk thresholds must use percentages or both sizes.', '兩個磁碟門檻須同為百分比或同為容量。');
  reject(minFree.value >= targetFree.value, 'disk', 'target-free', 0, lang, 'Cleanup target must be greater than the start threshold.', '清理目標須大於開始清理的門檻。');
  const minimum = scales.reduce((sum, scale) => sum + Number(scale.set['min-runners'] || 0), 0);
  reject(Number(state.global?.concurrent || 0) > 0 && Number(state.global.concurrent) < minimum, 'global', 'concurrent', 0, lang, 'Host-wide concurrency must cover all warm runner minimums.', '主機同時執行上限須涵蓋所有常駐 runner 下限。');

  const globalLines = emitFields(state.global, groups.global, lang, 'global', 0, state.global?.['log-file-disabled'] ? ['log-file'] : []);
  if (state.global?.['log-file-disabled']) globalLines.push('log-file = ""');
  const lines = ['# config.toml — add credentials locally before validation', ...globalLines];
  if (scales.length === 1) lines.push(...emitSet(scales[0], '', lang, 0));
  lines.push(...emitTable('actions-cache', state.actionsCache, groups['actions-cache'], lang, 'actionsCache'));
  lines.push(...emitTable('disk', state.disk, groups.disk, lang, 'disk'));
  lines.push(...emitTable('diagnostics', state.diagnostics, groups.diagnostics, lang, 'diagnostics'));
  if (scales.length > 1) scales.forEach((scale, index) => lines.push('', '[[scaleset]]', ...emitSet(scale, 'scaleset.', lang, index)));
  return `${lines.join('\n')}\n`;
}

function emitSet(scale, prefix, lang, index) {
  const set = scale.set;
  const lines = emitFields(set, groups.set, lang, 'set', index, ['auth', 'token-env', 'client-id', 'installation-id', 'app-key-source', 'key-file', 'key-env']);
  if (set.auth === 'pat') {
    const tokenEnv = copy(set['token-env']);
    lines.splice(2, 0, '# Set the PAT on the host.', `token = ${quoteField(tokenEnv ? `env:${tokenEnv}` : '', 'set', 'token-env', index)}`);
  } else {
    lines.push(`github-app.client-id = ${quoteField(copy(set['client-id']), 'set', 'client-id', index)}`, `github-app.installation-id = ${Number(set['installation-id'])}`);
    lines.push(set['app-key-source'] === 'env' ? `github-app.private-key-env = ${quoteField(copy(set['key-env']), 'set', 'key-env', index)}` : `github-app.private-key-file = ${quoteField(copy(set['key-file']), 'set', 'key-file', index)}`);
  }
  const nested = [
    ...emitTable(`${prefix}retry`, scale.retry, groups.retry, lang, 'retry', index),
    ...(set.provider === 'docker' ? emitTable(`${prefix}docker`, scale.docker, groups.docker, lang, 'docker', index) : emitTable(`${prefix}tart`, scale.tart, groups.tart, lang, 'tart', index)),
  ];
  if (set.provider === 'docker') nested.push(...emitLegacyCache(`${prefix}docker.cache`, scale.docker?.cache, lang, index));
  return [...lines, ...nested];
}
