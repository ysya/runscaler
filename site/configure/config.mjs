import { quoteToml } from './toml.mjs';
import { groups } from './schema.mjs';

const copy = value => String(value ?? '').trim();
const list = value => String(value ?? '').split(/\r?\n/u).map(item => item.trim()).filter(Boolean);
const fieldName = (field, lang) => lang === 'ja' ? field.ja : lang === 'zh-TW' ? field.zh : field.en;
const message = (lang, en, zh, ja) => lang === 'ja' ? ja : lang === 'zh-TW' ? zh : en;
const fieldError = (group, key, index, text) => Object.assign(new Error(text), { field: { group, key, index } });
const reject = (condition, group, key, index, lang, en, zh, ja) => {
  if (condition) throw fieldError(group, key, index, message(lang, en, zh, ja));
};
const volumeName = /^[a-zA-Z0-9][a-zA-Z0-9_.-]*$/u;
function quoteField(value, group, key, index, lang) {
  try { return quoteToml(value, lang); }
  catch (error) { throw fieldError(group, key, index, error.message); }
}

function cleanPath(value) {
  return value.startsWith('/') && value !== '/' && !value.endsWith('/') && !value.split('/').slice(1).some(part => !part || part === '.' || part === '..');
}

function pathsOverlap(a, b) {
  return a === b || a === '/' || b === '/' || a.startsWith(`${b}/`) || b.startsWith(`${a}/`);
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
  if (field.type === 'number-or-auto' && value === 'auto') return quoteToml(value, lang);
  if (field.type === 'number' || field.type === 'number-or-auto') {
    const number = Number(value);
    if (!Number.isSafeInteger(number) || (field.min !== undefined && number < field.min) || (field.max !== undefined && number > field.max)) {
      throw new Error(message(lang, `Invalid number: ${fieldName(field, lang)}`, `數值無效：${fieldName(field, lang)}`, `数値が無効です：${fieldName(field, lang)}`));
    }
    return String(number);
  }
  if (field.type === 'bool') {
    if (value !== 'true' && value !== 'false') throw new Error(message(lang, `Invalid boolean: ${field.key}`, `布林值無效：${field.key}`, `真偽値が無効です：${fieldName(field, lang)}`));
    return value;
  }
  if (field.type === 'select' && !field.choices.includes(value)) throw new Error(message(lang, `Invalid choice: ${field.key}`, `選項無效：${field.key}`, `選択肢が無効です：${fieldName(field, lang)}`));
  if (field.type === 'lines') return `[${list(raw).map(item => quoteToml(item, lang)).join(', ')}]`;
  const httpsOnly = ['url', 'token-issuer', 'jwks-url', 'upstream-results-url'].includes(field.key);
  if (field.type === 'url' && !validURL(value, httpsOnly ? ['https:'] : ['https:', 'http:'])) {
    throw new Error(message(lang, `Invalid URL: ${fieldName(field, lang)}`, `網址無效：${fieldName(field, lang)}`, `URL が無効です：${fieldName(field, lang)}`));
  }
  if (field.type === 'duration' && !/^(?:0|-?(?:\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h))+)$/u.test(value)) {
    throw new Error(message(lang, `Use a Go duration such as 5m or 24h: ${fieldName(field, lang)}`, `請輸入 Go 時間格式，例如 5m 或 24h：${fieldName(field, lang)}`, `5m や 24h などの Go の時間形式で入力してください：${fieldName(field, lang)}`));
  }
  return quoteToml(value, lang);
}

function emitFields(values = {}, fields, lang, group, index = 0, excluded = []) {
  return fields.filter(field => !excluded.includes(field.key)).flatMap(field => {
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

function fixedMinimum(set) {
  const value = copy(set['min-runners']);
  return value === 'auto' ? 0 : Number(value);
}

function validateSet(scale, global, lang, index) {
  const set = scale.set ?? {};
  const provider = copy(set.provider) || 'docker';
  const auth = copy(set.auth) || 'pat';
  const sweepMode = copy(set['registration-sweep-mode']) || 'off';
  reject(!['off', 'log', 'remove'].includes(sweepMode), 'set', 'registration-sweep-mode', index, lang, 'Invalid registration sweep mode.', '註冊清理模式無效。', 'ランナー登録のクリーンアップモードが無効です。');
  const sweepInterval = copy(set['registration-sweep-interval']);
  reject(sweepInterval && sweepInterval !== '0' && !(durationNs(sweepInterval) >= 60e9), 'set', 'registration-sweep-interval', index, lang, 'Registration sweep interval must be at least 1m.', '註冊清理間隔須至少為 1m。', 'ランナー登録のクリーンアップ間隔は 1m 以上にしてください。');
  reject(!validURL(copy(set.url), ['https:']) || new URL(set.url || 'https://github.com/').pathname === '/', 'set', 'url', index, lang, 'Enter an HTTPS GitHub organization or repository URL.', '請輸入 HTTPS GitHub 組織或儲存庫網址。', 'GitHub 組織またはリポジトリの HTTPS URL を入力してください。');
  reject(sweepMode !== 'off' && new URL(set.url).pathname.split('/').filter(Boolean).length !== 1, 'set', 'registration-sweep-mode', index, lang, 'Registration sweeping requires an organization URL.', '註冊清理需要組織網址。', 'ランナー登録のクリーンアップには組織の URL が必要です。');
  reject(!copy(set.name), 'set', 'name', index, lang, 'Enter a scale-set name.', '請輸入規模集名稱。', 'スケールセット名を入力してください。');
  reject(provider !== 'docker' && provider !== 'tart', 'set', 'provider', index, lang, 'Invalid provider.', '執行環境無效。', 'プロバイダーが無効です。');
  reject(auth !== 'pat' && auth !== 'app', 'set', 'auth', index, lang, 'Invalid authentication.', '驗證方式無效。', '認証方式が無効です。');
  const max = Number(set['max-runners']);
  const min = fixedMinimum(set);
  reject(!Number.isSafeInteger(max) || max < 1, 'set', 'max-runners', index, lang, 'Maximum runners must be a positive whole number.', '最多 runner 數量須為正整數。', 'ランナー数の上限には正の整数を指定してください。');
  reject(!Number.isSafeInteger(min) || min < 0 || min > max, 'set', 'min-runners', index, lang, 'Enter auto or a whole number from 0 to the maximum runner count.', '請輸入 auto，或 0 到 runner 上限之間的整數。', 'auto または 0 からランナー数の上限までの整数を指定してください。');
  reject(!copy(set['runner-image']), 'set', 'runner-image', index, lang, 'Enter a runner image.', '請輸入 runner 映像檔。', 'ランナーイメージを入力してください。');
  if (auth === 'app') {
    let path;
    try { path = decodeURIComponent(new URL(set.url).pathname); } catch { throw fieldError('set', 'url', index, message(lang, 'Invalid GitHub URL.', 'GitHub 網址無效。', 'GitHub URL が無効です。')); }
    reject(path.replace(/^\/+/, '').startsWith('enterprises/'), 'set', 'auth', index, lang, 'Use a PAT for enterprise-level runners.', '企業層級 runner 請使用 PAT。', 'エンタープライズレベルのランナーには PAT を使用してください。');
    reject(!copy(set['client-id']), 'set', 'client-id', index, lang, 'Enter the GitHub App client ID.', '請填寫 GitHub App 用戶端 ID。', 'GitHub App のクライアント ID を入力してください。');
    reject(!Number.isSafeInteger(Number(set['installation-id'])) || Number(set['installation-id']) < 1, 'set', 'installation-id', index, lang, 'Enter a positive installation ID.', '請填寫正整數安裝 ID。', 'インストール ID には正の整数を入力してください。');
    if (set['app-key-source'] === 'env') reject(!/^[A-Za-z_][A-Za-z0-9_]*$/u.test(copy(set['key-env'])), 'set', 'key-env', index, lang, 'Enter a valid environment variable name.', '請輸入有效的環境變數名稱。', '有効な環境変数名を入力してください。');
    else reject(!copy(set['key-file']).startsWith('/'), 'set', 'key-file', index, lang, 'Enter an absolute key-file path.', '請輸入私鑰檔案絕對路徑。', '秘密鍵ファイルの絶対パスを入力してください。');
  } else if (copy(set['token-env']) && (!/^[A-Za-z_][A-Za-z0-9_]*$/u.test(copy(set['token-env'])) || /^(?:ghp_|gho_|ghu_|ghs_|ghr_|github_pat_)/iu.test(copy(set['token-env'])))) {
    throw fieldError('set', 'token-env', index, message(lang, 'Enter a valid PAT environment variable name.', '請輸入有效的 PAT 環境變數名稱。', 'PAT の有効な環境変数名を入力してください。'));
  }
  const retry = scale.retry ?? {};
  const initial = durationNs(copy(retry.initial) || '5s');
  const retryMax = durationNs(copy(retry.max) || '1m');
  const cooldown = durationNs(copy(retry.cooldown) || '5m');
  reject(!(initial > 0), 'retry', 'initial', index, lang, 'Initial retry delay must be positive.', '初次重試間隔須為正數。', '最初の再試行までの待機時間は正の値にしてください。');
  reject(!(retryMax >= initial), 'retry', 'max', index, lang, 'Maximum retry delay must be at least the initial delay.', '最大重試間隔不可小於初次重試間隔。', '再試行の最大待機時間は最初の待機時間以上にしてください。');
  reject(!(cooldown >= retryMax), 'retry', 'cooldown', index, lang, 'Cooldown must be at least the maximum retry delay.', '冷卻時間不可小於最大重試間隔。', 'クールダウンは再試行の最大待機時間以上にしてください。');
  reject(copy(retry['cooldown-after']) && !(Number(retry['cooldown-after']) >= 1), 'retry', 'cooldown-after', index, lang, 'Cooldown failure count must be positive.', '進入冷卻前的失敗次數須為正數。', 'クールダウンに入るまでの失敗回数は正の値にしてください。');
  if (provider === 'docker') {
    const docker = scale.docker ?? {};
    const daemonMode = copy(docker['daemon-mode']) || (copy(docker.dind) === 'false' ? 'none' : 'host');
    reject(copy(docker['ready-timeout']) && !(durationNs(copy(docker['ready-timeout'])) > 0), 'docker', 'ready-timeout', index, lang, 'Readiness timeout must be positive.', '就緒逾時須為正數。', '準備完了の待機タイムアウトは正の値にしてください。');
    reject(copy(docker['dind-startup-timeout']) && !(durationNs(copy(docker['dind-startup-timeout'])) > 0), 'docker', 'dind-startup-timeout', index, lang, 'Private Docker startup timeout must be positive.', '私人 Docker daemon 啟動逾時須為正數。', '専用 Docker デーモンの起動タイムアウトは正の値にしてください。');
    reject(copy(docker['workspace-root']) && daemonMode !== 'host', 'docker', 'workspace-root', index, lang, 'Docker workspace root requires host Docker socket access.', 'Docker 工作目錄需要使用主機 Docker socket。', 'Docker のワークスペースにはホストの Docker ソケットへのアクセスが必要です。');
    reject(copy(docker['workspace-root']) && !cleanPath(copy(docker['workspace-root'])), 'docker', 'workspace-root', index, lang, 'Workspace root must be a clean absolute path other than /.', '工作目錄須為乾淨的絕對路徑，且不可為 /。', 'ワークスペースには / 以外の正規化された絶対パスを指定してください。');
    reject(copy(docker.platform) && !/^[^/]+\/[^/]+(?:\/[^/]+)?$/u.test(copy(docker.platform)), 'docker', 'platform', index, lang, 'Docker platform must be os/arch or os/arch/variant.', 'Docker 平台須為 os/arch 或 os/arch/variant。', 'Docker プラットフォームは os/arch または os/arch/variant で指定してください。');
    reject(copy(docker['shared-volume-name']) && !volumeName.test(copy(docker['shared-volume-name'])), 'docker', 'shared-volume-name', index, lang, 'Invalid shared volume name.', '共用 volume 名稱無效。', '共有ボリューム名が無効です。');
    const sharedPath = copy(docker['shared-volume']);
    reject(sharedPath && !cleanPath(sharedPath), 'docker', 'shared-volume', index, lang, 'Shared volume must be a clean absolute path other than /.', '共用 volume 須為乾淨的絕對路徑，且不可為 /。', '共有ボリュームには / 以外の正規化された絶対パスを指定してください。');
    if (daemonMode === 'dind') {
      const network = copy(docker.network);
      reject(network === 'host' || network.startsWith('container:'), 'docker', 'network', index, lang, 'Private Docker requires a private network namespace.', '私人 Docker daemon 需要獨立的網路命名空間。', '専用 Docker デーモンには独立したネットワーク名前空間が必要です。');
      reject(copy(docker.platform) && !copy(docker.platform).startsWith('linux/'), 'docker', 'platform', index, lang, 'Private Docker requires a Linux platform.', '私人 Docker daemon 需要 Linux 平台。', '専用 Docker デーモンには Linux プラットフォームが必要です。');
      const reserved = ['/var/run', '/run', '/var/lib/docker', '/home/runner/_work', '/home/runner/externals'];
      reject(sharedPath && reserved.some(target => pathsOverlap(sharedPath, target)), 'docker', 'shared-volume', index, lang, 'Shared volume overlaps a reserved runner or Docker daemon path.', '共用 volume 與 runner 或 Docker daemon 的保留路徑重疊。', '共有ボリュームのマウント先がランナーまたは Docker デーモンの予約済みパスと重複しています。');
    }
    const cache = global['actions-cache'];
    const receiverEnabled = copy(cache?.enabled) === 'true';
    const useNative = copy(docker['native-actions-cache']) === 'true';
    const cacheURL = copy(docker['actions-cache-url']);
    reject(useNative && !receiverEnabled, 'docker', 'native-actions-cache', index, lang, 'Enable the global native Actions cache receiver first.', '請先啟用全域內建 Actions 快取接收端。', '先にグローバル設定の内蔵 Actions キャッシュ受信機能を有効にしてください。');
    reject(cacheURL && (!validURL(cacheURL, ['http:', 'https:']) || !cacheURL.endsWith('/')), 'docker', 'actions-cache-url', index, lang, 'Actions cache URL must end with / and contain no credentials, query, or fragment.', 'Actions 快取網址須以 / 結尾，且不可包含憑證、查詢參數或片段。', 'Actions キャッシュの URL は末尾を / にし、認証情報・クエリ・フラグメントを含めないでください。');
    reject(cacheURL && receiverEnabled && cacheURL === copy(cache['advertise-url']), 'docker', 'actions-cache-url', index, lang, 'For the native receiver, enable native-actions-cache and leave the external URL blank.', '使用內建接收端時，請啟用 native-actions-cache 並將外部網址留白。', '内蔵キャッシュを使う場合は native-actions-cache を有効にし、外部 URL は空欄にしてください。');
    reject(cacheURL && useNative, 'docker', 'actions-cache-url', index, lang, 'Disable native-actions-cache to use an external receiver.', '使用外部接收端時，請停用 native-actions-cache。', '外部キャッシュを使う場合は native-actions-cache を無効にしてください。');
  }
  if (provider === 'tart') {
    const tart = scale.tart ?? {};
    reject(copy(tart['net-softnet']) === 'true' && copy(tart['net-bridged']), 'tart', 'net-softnet', index, lang, 'Softnet and bridged networking cannot both be enabled.', 'Softnet 與橋接網路不可同時啟用。', 'Softnet とブリッジネットワークは同時に有効にできません。');
    for (const key of ['net-softnet-allow', 'net-softnet-block']) {
      const rules = copy(tart[key]);
      reject(rules && copy(tart['net-softnet']) !== 'true', 'tart', key, index, lang, 'Softnet rules require Softnet networking.', 'Softnet 規則需要啟用 Softnet 網路。', 'Softnet のルールには Softnet ネットワークの有効化が必要です。');
      reject(rules && (/^-/u.test(rules) || /[\r\n=]/u.test(rules)), 'tart', key, index, lang, 'Invalid Softnet rule list.', 'Softnet 規則清單無效。', 'Softnet のルール一覧が無効です。');
    }
    reject(copy(tart['pool-size']) && Number(tart['pool-size']) > max, 'tart', 'pool-size', index, lang, 'Tart warm pool cannot exceed maximum runners.', 'Tart 預熱 VM 數量不可超過最多 runner 數量。', 'Tart のウォームプール数はランナー数の上限以下にしてください。');
    reject(copy(tart.mtu) && ![0, -1].includes(Number(tart.mtu)) && (Number(tart.mtu) < 1280 || Number(tart.mtu) > 1500), 'tart', 'mtu', index, lang, 'Tart MTU must be 0, -1, or 1280–1500.', 'Tart MTU 須為 0、-1 或 1280–1500。', 'Tart の MTU は 0、-1、または 1280～1500 を指定してください。');
    reject(list(tart.dns).some(ip => !validIP(ip)), 'tart', 'dns', index, lang, 'Tart DNS entries must be IP addresses.', 'Tart DNS 項目須為 IP 位址。', 'Tart の DNS には IP アドレスを指定してください。');
    reject(copy(tart['net-bridged']) && (/^-/u.test(copy(tart['net-bridged'])) || /[\s=]/u.test(copy(tart['net-bridged']))), 'tart', 'net-bridged', index, lang, 'Enter a valid host interface name.', '請輸入有效的主機網路介面名稱。', '有効なホストのインターフェース名を入力してください。');
  }
}

export function generateFullConfig(state, lang = 'en') {
  const scales = state.scaleSets ?? [];
  if (!scales.length) throw new Error(message(lang, 'Add a scale set.', '請新增規模集。', 'スケールセットを追加してください。'));
  const global = { ...state.global, 'actions-cache': state.actionsCache };
  scales.forEach((scale, index) => validateSet(scale, global, lang, index));
  const names = scales.map(scale => scale.set.name.trim());
  const duplicate = names.findIndex((name, index) => names.indexOf(name) !== index);
  reject(duplicate !== -1, 'set', 'name', duplicate, lang, 'Scale-set names must be unique.', '規模集名稱不可重複。', 'スケールセット名は重複しないようにしてください。');
  const cache = state.actionsCache ?? {};
  if (copy(cache.enabled) === 'true') {
    reject(!privateListen(copy(cache['listen-address']), state.global?.['health-port'] ?? '8080'), 'actionsCache', 'listen-address', 0, lang, 'Use a private IP and port different from the health port.', '請使用私人 IP 與連接埠，且不可與健康檢查連接埠相同。', 'プライベート IP と、ヘルスチェックとは異なるポートを指定してください。');
    reject(!validURL(copy(cache['advertise-url']), ['http:', 'https:'], true), 'actionsCache', 'advertise-url', 0, lang, 'Advertise URL must be a root HTTP(S) URL ending with /.', '公開網址須為以 / 結尾的 HTTP(S) 根路徑網址。', '公開する URL は末尾が / の HTTP(S) のルート URL にしてください。');
    reject(!cleanPath(copy(cache['data-dir'])), 'actionsCache', 'data-dir', 0, lang, 'Data directory must be a clean absolute private path other than /.', '資料目錄須為乾淨的私人絕對路徑，且不可為 /。', 'データディレクトリには / 以外の正規化された非公開の絶対パスを指定してください。');
    reject(!(Number(cache['max-size-bytes']) > 0), 'actionsCache', 'max-size-bytes', 0, lang, 'Cache byte limit must be positive.', '快取位元組上限須為正數。', 'キャッシュの容量上限は正の値にしてください。');
    reject(copy(cache['token-issuer']) && copy(cache['token-issuer']) !== 'https://token.actions.githubusercontent.com' && !copy(cache['jwks-url']), 'actionsCache', 'jwks-url', 0, lang, 'A custom token issuer requires a JWKS URL.', '自訂權杖簽發者需要 JWKS 網址。', 'カスタムトークン発行者には JWKS URL が必要です。');
  }
  const disk = state.disk ?? {};
  const minFree = threshold(copy(disk['min-free']) || '10%');
  const targetFree = threshold(copy(disk['target-free']) || '20%');
  reject(!minFree, 'disk', 'min-free', 0, lang, 'Use a percentage (10%) or size (20GB).', '請輸入百分比（10%）或容量（20GB）。', '割合（10%）または容量（20GB）を入力してください。');
  reject(!targetFree, 'disk', 'target-free', 0, lang, 'Use a percentage (20%) or size (40GB).', '請輸入百分比（20%）或容量（40GB）。', '割合（20%）または容量（40GB）を入力してください。');
  reject(minFree.type !== targetFree.type, 'disk', 'target-free', 0, lang, 'Both disk thresholds must use percentages or both sizes.', '兩個磁碟門檻須同為百分比或同為容量。', 'ディスクのしきい値は両方とも割合、または両方とも容量で指定してください。');
  reject(minFree.value >= targetFree.value, 'disk', 'target-free', 0, lang, 'Cleanup target must be greater than the start threshold.', '清理目標須大於開始清理的門檻。', 'クリーンアップ後の目標は開始しきい値より大きくしてください。');
  const minimum = scales.reduce((sum, scale) => sum + fixedMinimum(scale.set), 0);
  reject(Number(state.global?.concurrent || 0) > 0 && Number(state.global.concurrent) < minimum, 'global', 'concurrent', 0, lang, 'Host-wide concurrency must cover all warm runner minimums.', '主機同時執行上限須涵蓋所有常駐 runner 下限。', 'ホスト全体の同時実行数は、すべての待機ランナー数の下限の合計以上にしてください。');

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
    lines.splice(2, 0, '# Set the PAT on the host.', `token = ${quoteField(tokenEnv ? `env:${tokenEnv}` : '', 'set', 'token-env', index, lang)}`);
  } else {
    lines.push(`github-app.client-id = ${quoteField(copy(set['client-id']), 'set', 'client-id', index, lang)}`, `github-app.installation-id = ${Number(set['installation-id'])}`);
    lines.push(set['app-key-source'] === 'env' ? `github-app.private-key-env = ${quoteField(copy(set['key-env']), 'set', 'key-env', index, lang)}` : `github-app.private-key-file = ${quoteField(copy(set['key-file']), 'set', 'key-file', index, lang)}`);
  }
  return [
    ...lines,
    ...emitTable(`${prefix}retry`, scale.retry, groups.retry, lang, 'retry', index),
    ...(set.provider === 'docker' ? emitTable(`${prefix}docker`, scale.docker, groups.docker, lang, 'docker', index) : emitTable(`${prefix}tart`, scale.tart, groups.tart, lang, 'tart', index)),
  ];
}
