import { quoteToml } from './toml.mjs';
import { groups } from './schema.mjs';

const copy = value => String(value ?? '').trim();
const list = value => String(value ?? '').split(/\r?\n/u).map(item => item.trim()).filter(Boolean);
const fieldName = (field, lang) => lang === 'zh-TW' ? field.zh : field.en;
const message = (lang, en, zh) => lang === 'zh-TW' ? zh : en;

function validURL(value, schemes, rootOnly = false) {
  let parsed;
  try { parsed = new URL(value); } catch { return false; }
  return schemes.includes(parsed.protocol) && Boolean(parsed.hostname) && !parsed.username && !parsed.password && !parsed.search && !parsed.hash && (!rootOnly || parsed.pathname === '/') && (!rootOnly || value.endsWith('/'));
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
  if (field.type === 'duration' && !/^-?(?:\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h))+$/u.test(value)) {
    throw new Error(message(lang, `Use a Go duration such as 5m or 24h: ${fieldName(field, lang)}`, `請輸入 Go 時間格式，例如 5m 或 24h：${fieldName(field, lang)}`));
  }
  return quoteToml(value);
}

function emitFields(values = {}, fields, lang, excluded = []) {
  return fields.filter(field => !excluded.includes(field.key) && field.type !== 'cache').flatMap(field => {
    const encoded = serialize(field, values[field.key], lang);
    return encoded === null ? [] : [`${field.key} = ${encoded}`];
  });
}

function emitTable(header, values, fields, lang) {
  const lines = emitFields(values, fields, lang);
  return lines.length ? ['', `[${header}]`, ...lines] : [];
}

function emitLegacyCache(header, raw, lang) {
  return list(raw).flatMap((entry, index) => {
    const [name, path, budget = '', onExceed = '', ...rest] = entry.split('|').map(part => part.trim());
    if (rest.length || !/^[a-zA-Z0-9][a-zA-Z0-9_.-]*$/u.test(name) || !path?.startsWith('/') || (onExceed && !['warn', 'wipe'].includes(onExceed))) {
      throw new Error(message(lang, `Invalid legacy cache volume on line ${index + 1}.`, `第 ${index + 1} 行的舊版 cache volume 無效。`));
    }
    const lines = ['', `[[${header}]]`, `name = ${quoteToml(name)}`, `path = ${quoteToml(path)}`];
    if (budget) lines.push(`budget = ${quoteToml(budget)}`);
    if (onExceed) lines.push(`on-exceed = ${quoteToml(onExceed)}`);
    return lines;
  });
}

function validateSet(scale, global, lang) {
  const set = scale.set ?? {};
  const provider = copy(set.provider) || 'docker';
  const auth = copy(set.auth) || 'pat';
  if (!validURL(copy(set.url), ['https:']) || new URL(set.url).pathname === '/') throw new Error(message(lang, 'Enter an HTTPS GitHub organization or repository URL.', '請輸入 HTTPS GitHub 組織或儲存庫網址。'));
  if (!copy(set.name)) throw new Error(message(lang, 'Enter a scale-set name.', '請輸入規模集名稱。'));
  if (provider !== 'docker' && provider !== 'tart') throw new Error('Invalid provider.');
  if (auth !== 'pat' && auth !== 'app') throw new Error('Invalid authentication.');
  const max = Number(set['max-runners']);
  const min = copy(set['min-runners']) === '' ? 0 : Number(set['min-runners']);
  if (!Number.isSafeInteger(max) || max < 1 || !Number.isSafeInteger(min) || min < 0 || min > max) {
    throw new Error(message(lang, 'Runner limits must be whole numbers with 0 ≤ minimum ≤ maximum.', 'Runner 數量須為整數，且 0 ≤ 下限 ≤ 上限。'));
  }
  if (provider === 'tart' && max > 2) throw new Error(message(lang, 'Tart allows at most two concurrent VMs per host.', 'Tart 每台主機最多同時執行兩台 VM。'));
  if (!copy(set['runner-image'])) throw new Error(message(lang, 'Enter a runner image.', '請輸入 runner 映像檔。'));
  if (auth === 'app') {
    let path;
    try { path = decodeURIComponent(new URL(set.url).pathname); } catch { throw new Error(message(lang, 'Invalid GitHub URL.', 'GitHub 網址無效。')); }
    if (path.replace(/^\/+/, '').startsWith('enterprises/')) throw new Error(message(lang, 'Use a PAT for enterprise-level runners.', '企業層級 runner 請使用 PAT。'));
    if (!copy(set['client-id']) || !Number.isSafeInteger(Number(set['installation-id'])) || Number(set['installation-id']) < 1) {
      throw new Error(message(lang, 'Enter the GitHub App client ID and positive installation ID.', '請填寫 GitHub App 用戶端 ID 與正整數安裝 ID。'));
    }
    if (set['app-key-source'] === 'env' ? !/^[A-Za-z_][A-Za-z0-9_]*$/u.test(copy(set['key-env'])) : !copy(set['key-file']).startsWith('/')) throw new Error(message(lang, 'Enter an absolute key-file path or a valid environment variable name.', '請輸入私鑰檔案絕對路徑或有效的環境變數名稱。'));
  } else if (copy(set['token-env']) && (!/^[A-Za-z_][A-Za-z0-9_]*$/u.test(copy(set['token-env'])) || /^(?:ghp_|gho_|ghu_|ghs_|ghr_|github_pat_)/iu.test(copy(set['token-env'])))) {
    throw new Error(message(lang, 'Enter a valid PAT environment variable name.', '請輸入有效的 PAT 環境變數名稱。'));
  }
  if (provider === 'docker') {
    const docker = scale.docker ?? {};
    if (copy(docker['workspace-root']) && copy(docker.dind) === 'false') throw new Error(message(lang, 'Docker workspace root requires Docker socket access.', 'Docker 工作目錄需要開啟 Docker socket 存取。'));
    if (copy(docker['native-actions-cache']) === 'true' && copy(global['actions-cache']?.enabled) !== 'true') throw new Error(message(lang, 'Enable the global native Actions cache receiver first.', '請先啟用全域內建 Actions 快取接收端。'));
    if (copy(docker['actions-cache-url']) && (!validURL(copy(docker['actions-cache-url']), ['http:', 'https:']) || !copy(docker['actions-cache-url']).endsWith('/'))) throw new Error(message(lang, 'Actions cache URL must end with / and contain no credentials, query, or fragment.', 'Actions 快取網址須以 / 結尾，且不可包含憑證、查詢參數或片段。'));
    if (copy(docker['native-actions-cache']) === 'true' && copy(docker['actions-cache-url']) && docker['actions-cache-url'] !== global['actions-cache']?.['advertise-url']) throw new Error(message(lang, 'Native and explicit cache URLs must match.', '內建與明確指定的快取網址必須相同。'));
  }
  if (provider === 'tart') {
    const tart = scale.tart ?? {};
    if (copy(tart['pool-size']) && Number(tart['pool-size']) > max) throw new Error(message(lang, 'Tart warm pool cannot exceed maximum runners.', 'Tart 預熱 VM 數量不可超過最多 runner 數量。'));
    if (copy(tart.mtu) && ![0, -1].includes(Number(tart.mtu)) && (Number(tart.mtu) < 1280 || Number(tart.mtu) > 1500)) throw new Error(message(lang, 'Tart MTU must be 0, -1, or 1280–1500.', 'Tart MTU 須為 0、-1 或 1280–1500。'));
  }
}

export function generateFullConfig(state, lang = 'en') {
  const scales = state.scaleSets ?? [];
  if (!scales.length) throw new Error(message(lang, 'Add a scale set.', '請新增規模集。'));
  const global = { ...state.global, 'actions-cache': state.actionsCache };
  scales.forEach(scale => validateSet(scale, global, lang));
  const names = scales.map(scale => scale.set.name.trim());
  if (new Set(names).size !== names.length) throw new Error(message(lang, 'Scale-set names must be unique.', '規模集名稱不可重複。'));
  const cache = state.actionsCache ?? {};
  if (copy(cache.enabled) === 'true') {
    if (!copy(cache['listen-address']) || !validURL(copy(cache['advertise-url']), ['http:', 'https:'], true) || !copy(cache['data-dir']).startsWith('/') || !(Number(cache['max-size-bytes']) > 0)) {
      throw new Error(message(lang, 'Native cache needs a private listen address, root advertise URL, absolute data directory, and positive byte limit.', '內建快取需要私人監聽位址、根路徑公開網址、絕對資料目錄與正數容量上限。'));
    }
  }
  const minimum = scales.reduce((sum, scale) => sum + Number(scale.set['min-runners'] || 0), 0);
  if (Number(state.global?.concurrent || 0) > 0 && Number(state.global.concurrent) < minimum) throw new Error(message(lang, 'Host-wide concurrency must cover all warm runner minimums.', '主機同時執行上限須涵蓋所有常駐 runner 下限。'));

  const globalLines = emitFields(state.global, groups.global, lang, state.global?.['log-file-disabled'] ? ['log-file'] : []);
  if (state.global?.['log-file-disabled']) globalLines.push('log-file = ""');
  const lines = ['# config.toml — add credentials locally before validation', ...globalLines];
  if (scales.length === 1) lines.push(...emitSet(scales[0], '', lang));
  lines.push(...emitTable('actions-cache', state.actionsCache, groups['actions-cache'], lang));
  lines.push(...emitTable('disk', state.disk, groups.disk, lang));
  lines.push(...emitTable('diagnostics', state.diagnostics, groups.diagnostics, lang));
  if (scales.length > 1) scales.forEach(scale => lines.push('', '[[scaleset]]', ...emitSet(scale, 'scaleset.', lang)));
  return `${lines.join('\n')}\n`;
}

function emitSet(scale, prefix, lang) {
  const set = scale.set;
  const lines = emitFields(set, groups.set, lang, ['auth', 'token-env', 'client-id', 'installation-id', 'app-key-source', 'key-file', 'key-env']);
  if (set.auth === 'pat') {
    const tokenEnv = copy(set['token-env']);
    lines.splice(2, 0, '# Set the PAT on the host.', `token = ${quoteToml(tokenEnv ? `env:${tokenEnv}` : '')}`);
  } else {
    lines.push(`github-app.client-id = ${quoteToml(copy(set['client-id']))}`, `github-app.installation-id = ${Number(set['installation-id'])}`);
    lines.push(set['app-key-source'] === 'env' ? `github-app.private-key-env = ${quoteToml(copy(set['key-env']))}` : `github-app.private-key-file = ${quoteToml(copy(set['key-file']))}`);
  }
  const nested = [
    ...emitTable(`${prefix}retry`, scale.retry, groups.retry, lang),
    ...(set.provider === 'docker' ? emitTable(`${prefix}docker`, scale.docker, groups.docker, lang) : emitTable(`${prefix}tart`, scale.tart, groups.tart, lang)),
  ];
  if (set.provider === 'docker') nested.push(...emitLegacyCache(`${prefix}docker.cache`, scale.docker?.cache, lang));
  return [...lines, ...nested];
}
