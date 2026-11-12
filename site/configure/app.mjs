import { groups } from './schema.mjs';
import { generateFullConfig } from './config.mjs';

const lang = document.body.dataset.lang === 'zh-TW' ? 'zh-TW' : 'en';
const t = (en, zh) => lang === 'zh-TW' ? zh : en;
const defaults = fields => Object.fromEntries(fields.filter(field => field.value !== undefined).map(field => [field.key, field.value]));
const newSet = () => ({ set: defaults(groups.set), docker: defaults(groups.docker), tart: defaults(groups.tart), retry: {} });
const state = { global: defaults(groups.global), actionsCache: {}, disk: {}, diagnostics: {}, scaleSets: [newSet()] };
const form = document.getElementById('builder-form');
const scaleList = document.getElementById('scale-list');
const output = document.getElementById('config-output');
const message = document.getElementById('form-message');
const copyButton = document.getElementById('copy-config');
const copyStatus = document.getElementById('copy-status');
let currentConfig = '';

function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function renderField(field, group, index, values) {
  const wrapper = element('div', `tool-field${field.legacy ? ' legacy-field' : ''}`);
  wrapper.dataset.visible = field.visible || '';
  const id = `${group}-${index}-${field.key}`;
  const label = element('label', '', lang === 'zh-TW' ? field.zh : field.en);
  label.htmlFor = id;
  wrapper.append(label);
  let input;
  if (field.type === 'bool' || field.type === 'select') {
    input = element('select');
    const options = field.type === 'bool' ? [['', t('Use default', '使用預設值')], ['true', t('On', '開啟')], ['false', t('Off', '關閉')]] : field.choices.map(choice => [choice, choice]);
    for (const [value, title] of options) {
      const option = element('option', '', title);
      option.value = value;
      input.append(option);
    }
  } else if (field.type === 'lines' || field.type === 'cache') {
    input = element('textarea');
    input.rows = field.type === 'cache' ? 3 : 2;
  } else {
    input = element('input');
    input.type = field.type === 'number' ? 'number' : field.type === 'url' ? 'url' : 'text';
    if (field.min !== undefined) input.min = String(field.min);
    if (field.max !== undefined) input.max = String(field.max);
    if (field.type === 'number') input.step = '1';
  }
  input.id = id;
  input.dataset.group = group;
  input.dataset.index = String(index);
  input.dataset.key = field.key;
  input.autocomplete = 'off';
  input.spellcheck = false;
  input.value = values[field.key] ?? '';
  if (field.placeholder) input.placeholder = field.placeholder;
  wrapper.append(input);
  if (field.helpEn || field.helpZh) wrapper.append(element('p', 'field-help', t(field.helpEn, field.helpZh)));
  if (field.legacy) wrapper.append(element('p', 'legacy-note', t('Sunset: supported for existing configs; prefer workflow Actions cache for new deployments.', '已淘汰：僅為現有設定保留；新部署建議使用工作流程 Actions 快取。')));
  if (field.key === 'log-file') {
    const disable = element('label', 'inline-check');
    const check = element('input');
    check.type = 'checkbox';
    check.dataset.group = group;
    check.dataset.index = String(index);
    check.dataset.key = 'log-file-disabled';
    check.checked = values['log-file-disabled'] === true;
    disable.append(check, document.createTextNode(t(' Disable file logging', ' 停用檔案紀錄')));
    wrapper.append(disable);
  }
  return wrapper;
}

function appendFields(container, fields, group, index, values) {
  container.replaceChildren(...fields.map(field => renderField(field, group, index, values)));
}

function details(title, description, group, index, values, fields, open = false) {
  const box = element('details', 'set-details');
  box.open = open;
  box.append(element('summary', '', title));
  if (description) box.append(element('p', 'details-help', description));
  const grid = element('div', 'field-grid');
  appendFields(grid, fields, group, index, values);
  box.append(grid);
  return box;
}

function renderSets() {
  const oldOpen = [...scaleList.querySelectorAll('details')].map(detail => detail.open);
  scaleList.replaceChildren();
  state.scaleSets.forEach((scale, index) => {
    const card = element('article', 'scale-card');
    const heading = element('div', 'scale-heading');
    const title = element('h3', '', `${t('Scale set', '規模集')} ${index + 1}`);
    heading.append(title);
    if (state.scaleSets.length > 1) {
      const remove = element('button', 'remove-button', t('Remove', '移除'));
      remove.type = 'button';
      remove.dataset.remove = String(index);
      heading.append(remove);
    }
    card.append(heading);
    const basics = element('div', 'field-grid');
    appendFields(basics, groups.set, 'set', index, scale.set);
    card.append(basics);
    const docker = details(t('Docker options and cache', 'Docker 選項與快取'), t('Includes external and native Actions cache, Docker cleanup, and sunset cache volumes.', '包含外部及內建 Actions 快取、Docker 清理與已淘汰的 cache volume。'), 'docker', index, scale.docker, groups.docker, oldOpen[index * 3] ?? false);
    docker.dataset.provider = 'docker';
    const tart = details(t('Tart options and cache', 'Tart 選項與快取'), t('VM resources, network, warm pool, and OCI/IPSW cache retention.', 'VM 資源、網路、預熱池與 OCI/IPSW 快取保留策略。'), 'tart', index, scale.tart, groups.tart, oldOpen[index * 3 + 1] ?? false);
    tart.dataset.provider = 'tart';
    card.append(docker, tart, details(t('Retry behavior', '重試設定'), '', 'retry', index, scale.retry, groups.retry, oldOpen[index * 3 + 2] ?? false));
    scaleList.append(card);
  });
  updateVisibility();
}

function updateVisibility() {
  state.scaleSets.forEach((scale, index) => {
    const card = scaleList.children[index];
    const provider = scale.set.provider || 'docker';
    const auth = scale.set.auth || 'pat';
    card.querySelectorAll('[data-provider]').forEach(box => { box.hidden = box.dataset.provider !== provider; });
    card.querySelectorAll('[data-visible]').forEach(box => {
      const visible = box.dataset.visible;
      if (visible === 'app') box.hidden = auth !== 'app';
      else if (visible === 'pat') box.hidden = auth !== 'pat';
      else if (visible === 'app-file') box.hidden = auth !== 'app' || scale.set['app-key-source'] === 'env';
      else if (visible === 'app-env') box.hidden = auth !== 'app' || scale.set['app-key-source'] !== 'env';
    });
  });
}

function exampleState() {
  const sample = structuredClone(state);
  sample.scaleSets.forEach((scale, index) => {
    if (!scale.set.url) scale.set.url = 'https://github.com/your-org';
    if (!scale.set.name) scale.set.name = index ? `my-runners-${index + 1}` : 'my-runners';
    if (!scale.set['runner-image']) scale.set['runner-image'] = scale.set.provider === 'tart' ? 'ghcr.io/cirruslabs/macos-golden-gate-xcode:latest' : 'ghcr.io/actions/actions-runner:latest';
  });
  return sample;
}

function renderPreview() {
  copyStatus.textContent = '';
  const incomplete = state.scaleSets.some(scale => !scale.set.url?.trim() || !scale.set.name?.trim());
  try {
    const config = generateFullConfig(incomplete ? exampleState() : state, lang);
    output.textContent = incomplete ? `${t('# Example only — enter a GitHub URL and scale-set name to copy.', '# 範例設定；填寫 GitHub 網址與規模集名稱後才能複製。')}\n${config}` : config;
    currentConfig = incomplete ? '' : config;
    copyButton.disabled = incomplete;
    message.textContent = incomplete ? t('Example preview. Complete the required fields to copy your config.', '目前顯示範例；填寫必填欄位後即可複製設定檔。') : t('Ready to copy. Add credentials only on your host.', '可以複製；請只在主機上填入憑證。');
    message.classList.toggle('is-error', false);
  } catch (error) {
    currentConfig = '';
    copyButton.disabled = true;
    output.textContent = t('# Fix the form error above to generate config.toml.', '# 請修正上方的欄位錯誤以產生 config.toml。');
    message.textContent = error.message;
    message.classList.toggle('is-error', true);
  }
}

function onField(event) {
  const input = event.target;
  if (!input.dataset.group) return;
  const group = input.dataset.group;
  const index = Number(input.dataset.index);
  const values = group === 'set' || group === 'docker' || group === 'tart' || group === 'retry' ? state.scaleSets[index][group] : state[group];
  values[input.dataset.key] = input.type === 'checkbox' ? input.checked : input.value;
  if (group === 'set' && input.dataset.key === 'provider') {
    const scale = state.scaleSets[index];
    if (input.value === 'tart') {
      if (scale.set['max-runners'] === '10') scale.set['max-runners'] = '2';
      if (scale.set['runner-image'] === 'ghcr.io/actions/actions-runner:latest') scale.set['runner-image'] = '';
    } else {
      if (scale.set['max-runners'] === '2') scale.set['max-runners'] = '10';
      if (!scale.set['runner-image']) scale.set['runner-image'] = 'ghcr.io/actions/actions-runner:latest';
    }
    renderSets();
  } else if (group === 'set' && ['auth', 'app-key-source'].includes(input.dataset.key)) updateVisibility();
  renderPreview();
}

appendFields(document.getElementById('native-cache-fields'), groups['actions-cache'], 'actionsCache', 0, state.actionsCache);
appendFields(document.getElementById('disk-fields'), groups.disk, 'disk', 0, state.disk);
appendFields(document.getElementById('global-fields'), groups.global, 'global', 0, state.global);
appendFields(document.getElementById('diagnostics-fields'), groups.diagnostics, 'diagnostics', 0, state.diagnostics);
renderSets();
renderPreview();
form.addEventListener('submit', event => event.preventDefault());
form.addEventListener('input', onField);
form.addEventListener('change', onField);
scaleList.addEventListener('click', event => {
  const index = event.target.dataset.remove;
  if (index === undefined) return;
  state.scaleSets.splice(Number(index), 1);
  renderSets();
  renderPreview();
});
document.getElementById('add-set').addEventListener('click', () => {
  state.scaleSets.push(newSet());
  renderSets();
  renderPreview();
  scaleList.lastElementChild.querySelector('input')?.focus();
});
copyButton.addEventListener('click', async () => {
  if (!currentConfig) return;
  try {
    await navigator.clipboard.writeText(currentConfig);
    copyStatus.textContent = t('TOML copied.', '已複製 TOML。');
  } catch {
    const range = document.createRange();
    range.selectNodeContents(output);
    const selection = window.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
    copyStatus.textContent = t('Select the preview text and copy it manually.', '請選取預覽文字並手動複製。');
  }
});
