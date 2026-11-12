import { generateConfig } from './config.mjs';

const zh = document.body.dataset.lang === 'zh-TW';
const strings = zh ? {
  start: '輸入 GitHub 網址與規模集名稱，即可預覽設定檔。',
  ready: '設定檔可以複製；請在主機上補上憑證。',
  copied: '已複製 TOML。',
  copyFailed: '無法自動複製；請選取預覽文字後手動複製。',
  dockerCapacity: '請依主機可用資源設定。',
  tartCapacity: '每台 Apple Silicon 主機最多可同時執行兩台 macOS VM。',
  dockerImage: '已安裝 GitHub Actions runner 的 Docker 映像檔。',
  tartImage: '請選擇與主機 macOS 版本相容、已預裝 runner 的 Tart 映像檔。',
  patNote: '複製的檔案會保留空白的 token = ""；請在主機上填入 PAT 後再驗證。',
  appNote: '此檔案只包含私鑰路徑。請在主機上放置私鑰檔案，並確認服務可以讀取。',
  errors: {
    'Choose Docker or Tart.': '請選擇 Docker 或 Tart。',
    'Enter a valid HTTPS GitHub registration URL.': '請輸入有效的 HTTPS GitHub 組織或儲存庫網址。',
    'Enter an HTTPS GitHub organization or repository URL without credentials, query, or fragment.': '請輸入不含憑證、查詢參數或片段的 HTTPS GitHub 組織或儲存庫網址。',
    'Enter a scale-set name.': '請輸入規模集名稱。',
    'Maximum runners must be a positive whole number.': '最多 runner 數量必須是正整數。',
    'Tart supports at most 2 concurrent VMs per host.': 'Tart 每台主機最多同時執行兩台 VM。',
    'Enter a runner image.': '請輸入 runner 映像檔。',
    'GitHub App authentication is unavailable for enterprise-level runners; use a PAT.': '企業層級 runner 不支援 GitHub App 驗證；請改用 PAT。',
    'Enter the GitHub App client ID.': '請輸入 GitHub App 用戶端 ID。',
    'Enter a positive GitHub App installation ID.': '請輸入正整數的 GitHub App 安裝 ID。',
    'Enter an absolute private-key file path.': '請輸入私鑰檔案的絕對路徑。',
    'Choose PAT or GitHub App authentication.': '請選擇 PAT 或 GitHub App 驗證方式。',
    'Values cannot contain control characters.': '欄位不能包含換行或控制字元。',
    'Invalid Unicode in value.': '欄位包含無效的 Unicode 字元。',
  },
} : {
  start: 'Enter a GitHub URL and scale-set name to preview your config.',
  ready: 'Ready to copy. Add the credential on your host.',
  copied: 'TOML copied.',
  copyFailed: 'Automatic copy failed. Select the preview text and copy it manually.',
  dockerCapacity: 'Start with a number your host can comfortably run.',
  tartCapacity: 'An Apple Silicon host can run at most two concurrent macOS VMs.',
  dockerImage: 'A Docker image with the GitHub Actions runner installed.',
  tartImage: 'Choose a Tart image with a runner that matches your host macOS version.',
  patNote: 'The copied file leaves token = "" empty. Add your PAT locally before validating.',
  appNote: 'The file contains only a private-key path. Put the key on your host where the service can read it.',
  errors: {},
};

const form = document.getElementById('config-form');
const capacity = document.getElementById('max-runners');
const image = document.getElementById('runner-image');
const dockerOptions = document.getElementById('docker-options');
const appFields = document.getElementById('app-fields');
const dockerSteps = document.getElementById('docker-steps');
const tartSteps = document.getElementById('tart-steps');
const output = document.getElementById('config-output');
const copyButton = document.getElementById('copy-config');
const formMessage = document.getElementById('form-message');
const copyStatus = document.getElementById('copy-status');
const credentialNote = document.getElementById('credential-note');
const workflow = document.getElementById('workflow-output');
let currentConfig = '';

function selected(name) {
  return form.querySelector(`input[name="${name}"]:checked`)?.value;
}

function render() {
  const provider = selected('provider');
  const auth = selected('auth');
  dockerOptions.hidden = provider !== 'docker';
  dockerSteps.hidden = provider !== 'docker';
  tartSteps.hidden = provider !== 'tart';
  appFields.hidden = auth !== 'app';
  document.getElementById('capacity-help').textContent = provider === 'tart' ? strings.tartCapacity : strings.dockerCapacity;
  document.getElementById('image-help').textContent = provider === 'tart' ? strings.tartImage : strings.dockerImage;
  credentialNote.textContent = auth === 'app' ? strings.appNote : strings.patNote;

  const name = document.getElementById('scale-name').value.trim();
  workflow.textContent = `runs-on: ${JSON.stringify(name || 'my-runners')}`;
  const input = {
    provider,
    auth,
    url: document.getElementById('registration-url').value,
    name,
    maxRunners: capacity.value,
    runnerImage: image.value,
    dind: document.getElementById('dind').checked,
    clientID: document.getElementById('client-id').value,
    installationID: document.getElementById('installation-id').value,
    privateKeyFile: document.getElementById('key-file').value,
  };

  copyStatus.textContent = '';
  if (!input.url && !name) {
    currentConfig = '';
    output.textContent = '# config.toml';
    copyButton.disabled = true;
    formMessage.textContent = strings.start;
    formMessage.classList.remove('is-error');
    return;
  }
  try {
    currentConfig = generateConfig(input);
    output.textContent = currentConfig;
    copyButton.disabled = false;
    formMessage.textContent = strings.ready;
    formMessage.classList.remove('is-error');
  } catch (error) {
    currentConfig = '';
    output.textContent = '# Complete the form to generate config.toml';
    copyButton.disabled = true;
    formMessage.textContent = strings.errors[error.message] || error.message;
    formMessage.classList.add('is-error');
  }
}

form.addEventListener('submit', event => event.preventDefault());
form.addEventListener('input', render);
form.addEventListener('change', event => {
  if (event.target.name === 'provider') {
    const tart = event.target.value === 'tart';
    capacity.value = tart ? '2' : '10';
    capacity.max = tart ? '2' : '';
    image.value = tart ? '' : 'ghcr.io/actions/actions-runner:latest';
    image.placeholder = tart ? 'ghcr.io/cirruslabs/macos-tahoe-xcode:latest' : '';
    document.getElementById('key-file').placeholder = tart ? '/Users/you/.config/runner/app.pem' : '/etc/runner/app.pem';
  }
  render();
});

copyButton.addEventListener('click', async () => {
  if (!currentConfig) return;
  try {
    await navigator.clipboard.writeText(currentConfig);
    copyStatus.textContent = strings.copied;
  } catch {
    const range = document.createRange();
    range.selectNodeContents(output);
    const selection = window.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
    copyStatus.textContent = strings.copyFailed;
  }
});

render();
