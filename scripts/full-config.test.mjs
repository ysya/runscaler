import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { generateFullConfig } from '../site/configure/config.mjs';
import { groups } from '../site/configure/schema.mjs';
import { quoteToml } from '../site/configure/toml.mjs';

const baseSet = () => ({
  set: { url: 'https://github.com/example-org', name: 'linux-runners', provider: 'docker', auth: 'pat', 'max-runners': '10', 'runner-image': 'ghcr.io/actions/actions-runner:latest' },
  docker: { dind: 'true' }, tart: {}, retry: {},
});
const base = () => ({ global: { 'log-level': 'info', 'log-format': 'text', 'health-port': '8080', 'health-address': '127.0.0.1' }, actionsCache: {}, disk: {}, diagnostics: {}, scaleSets: [baseSet()] });
const fieldError = (state, group, key, index = 0) => assert.throws(
  () => generateFullConfig(state),
  error => error.field?.group === group && error.field?.key === key && error.field?.index === index,
  `${group}.${key} should identify the invalid field`,
);

test('TOML quoting escapes strings and rejects control characters', () => {
  assert.equal(quoteToml('a"\\b'), '"a\\"\\\\b"');
  assert.throws(() => quoteToml('x"\nmax-runners = 99'), /control characters/u);
});

test('all runtime config fields are represented in the builder schema', () => {
  const config = readFileSync(new URL('../internal/config/config.go', import.meta.url), 'utf8');
  const cache = readFileSync(new URL('../internal/config/actions_cache.go', import.meta.url), 'utf8');
  const retry = readFileSync(new URL('../internal/config/reliability.go', import.meta.url), 'utf8');
  const auth = readFileSync(new URL('../internal/config/auth.go', import.meta.url), 'utf8');
  for (const [source, struct, group, excluded] of [
    [config, 'Config', 'global', ['-', ',squash', 'disk', 'diagnostics', 'actions-cache', 'scaleset']],
    [config, 'ScaleSetConfig', 'set', ['token', 'github-app', 'retry', 'docker', 'tart']],
    [config, 'DockerConfig', 'docker', []],
    [config, 'TartConfig', 'tart', []],
    [config, 'DiskConfig', 'disk', []],
    [cache, 'ActionsCacheConfig', 'actions-cache', []],
    [retry, 'RetryConfig', 'retry', []],
    [retry, 'DiagnosticsConfig', 'diagnostics', []],
    [auth, 'GitHubAppConfig', 'set', ['private-key-file', 'private-key-env']],
  ]) {
    const body = source.match(new RegExp(`type ${struct} struct \\{([\\s\\S]*?)\\n\\}`))?.[1];
    assert.ok(body, struct);
    const keys = [...body.matchAll(/mapstructure:"([^"]+)"/gu)].map(match => match[1]).filter(key => !excluded.includes(key));
    const covered = new Set(groups[group].map(field => field.key));
    for (const key of keys) assert.ok(covered.has(key), `${struct}.${key} missing from ${group}`);
  }
});

test('default form emits a complete but credential-free single-scale-set TOML', () => {
  const toml = generateFullConfig(base());
  assert.match(toml, /^token = ""$/mu);
  assert.match(toml, /^\[docker\]$/mu);
  assert.match(toml, /^dind = true$/mu);
  assert.doesNotMatch(toml, /\[\[scaleset\]\]/u);
});

test('multi-scale-set config keeps global and nested tables in their proper scope', () => {
  const state = base();
  const tart = baseSet();
  tart.set = { ...tart.set, name: 'macos-runners', provider: 'tart', 'max-runners': '2', 'runner-image': 'ghcr.io/cirruslabs/macos-golden-gate-xcode:latest', auth: 'app', 'client-id': 'Iv1.example', 'installation-id': '123', 'app-key-source': 'env', 'key-env': 'RUNNER_APP_KEY' };
  tart.tart = { 'pool-size': '1', 'cache-budget': '80', 'cache-cleanup': 'true' };
  state.scaleSets.push(tart);
  state.global.concurrent = '12';
  const toml = generateFullConfig(state);
  assert.equal((toml.match(/\[\[scaleset\]\]/gu) || []).length, 2);
  assert.match(toml, /\[scaleset\.docker\]\ndind = true/u);
  assert.match(toml, /\[scaleset\.tart\][\s\S]*cache-budget = 80/u);
  assert.match(toml, /github-app\.private-key-env = "RUNNER_APP_KEY"/u);
  assert.doesNotMatch(toml.split('[[scaleset]]')[0], /^url =/mu);
});

test('native and legacy caches, disk guard, retry and diagnostics serialize correctly', () => {
  const state = base();
  state.actionsCache = { enabled: 'true', 'listen-address': '172.18.0.1:32500', 'advertise-url': 'http://cache.internal:32500/', 'data-dir': '/var/lib/runner/native-actions-cache', 'max-size-bytes': '53687091200', 'max-idle-days': '30' };
  state.scaleSets[0].docker = { dind: 'false', 'native-actions-cache': 'true', 'cache-volumes': 'gradle:/home/runner/.gradle', cache: 'pip | /home/runner/.cache/pip | 20GB | warn', 'shared-volume-max-age': '168h' };
  state.scaleSets[0].retry = { initial: '5s', max: '1m', 'cooldown-after': '5', cooldown: '5m' };
  state.disk = { guard: 'true', 'min-free': '10%', 'target-free': '20%', interval: '1h', 'max-tier': '4' };
  state.diagnostics = { enabled: 'true', 'capture-logs': 'true' };
  const toml = generateFullConfig(state);
  for (const fragment of ['[actions-cache]', 'native-actions-cache = true', 'cache-volumes = ["gradle:/home/runner/.gradle"]', '[[docker.cache]]', 'budget = "20GB"', '[disk]', 'max-tier = 4', '[diagnostics]', '[retry]']) assert.ok(toml.includes(fragment), fragment);
});

test('local credential references and explicit file-log disable serialize without secrets', () => {
  const state = base();
  state.global['log-file-disabled'] = true;
  state.scaleSets[0].set['token-env'] = 'RUNNER_PAT';
  let toml = generateFullConfig(state);
  assert.match(toml, /^token = "env:RUNNER_PAT"$/mu);
  assert.match(toml, /^log-file = ""$/mu);
  state.scaleSets[0].set = { ...state.scaleSets[0].set, auth: 'app', 'client-id': 'Iv1.example', 'installation-id': '123', 'app-key-source': 'file', 'key-file': '/etc/runner/app.pem' };
  toml = generateFullConfig(state);
  assert.match(toml, /^github-app.private-key-file = "\/etc\/runner\/app.pem"$/mu);
  assert.doesNotMatch(toml, /^token =/mu);
});

test('invalid or unsafe combinations cannot be copied', () => {
  const state = base();
  const credentialURL = new URL('https://github.com/org');
  credentialURL.username = 'name';
  credentialURL.password = 'example';
  state.scaleSets[0].set.url = credentialURL.toString();
  assert.throws(() => generateFullConfig(state), /GitHub organization/u);
  state.scaleSets[0].set.url = 'https://github.com/org';
  state.scaleSets[0].docker['native-actions-cache'] = 'true';
  assert.throws(() => generateFullConfig(state), /global native/u);
  state.scaleSets[0].docker['native-actions-cache'] = '';
  state.scaleSets[0].docker['actions-cache-url'] = 'https://cache.example/?token=secret';
  assert.throws(() => generateFullConfig(state), /Actions cache URL/u);
  state.scaleSets[0].docker['actions-cache-url'] = '';
  state.scaleSets[0].set['token-env'] = 'github_pat_example';
  assert.throws(() => generateFullConfig(state), /environment variable name/u);
  state.scaleSets[0].set['token-env'] = '';
  state.scaleSets[0].set.name = 'x"\nmax-runners = 999';
  assert.throws(() => generateFullConfig(state), /control character/u);
});

test('native cache validates the actual listen address and related fields', () => {
  const state = base();
  state.actionsCache = { enabled: 'true', 'listen-address': '172.18.0.1:32500', 'advertise-url': 'http://cache.internal:32500/', 'data-dir': '/var/lib/runner/cache', 'max-size-bytes': '1024' };
  state.actionsCache['listen-address'] = 'not-an-address';
  fieldError(state, 'actionsCache', 'listen-address');
  state.actionsCache['listen-address'] = '8.8.8.8:32500';
  fieldError(state, 'actionsCache', 'listen-address');
  state.actionsCache['listen-address'] = '172.18.0.1:8080';
  fieldError(state, 'actionsCache', 'listen-address');
  state.actionsCache['listen-address'] = '172.18.0.1:32500';
  state.actionsCache['data-dir'] = '/';
  fieldError(state, 'actionsCache', 'data-dir');
  state.actionsCache['data-dir'] = '/var/lib/runner/cache';
  state.actionsCache['token-issuer'] = 'https://issuer.example';
  fieldError(state, 'actionsCache', 'jwks-url');
  state.actionsCache['jwks-url'] = 'https://issuer.example/jwks';
  assert.match(generateFullConfig(state), /jwks-url = "https:\/\/issuer.example\/jwks"/u);
});

test('disk thresholds use compatible formats and increasing values', () => {
  const state = base();
  state.disk = { 'min-free': '30%', 'target-free': '20%' };
  fieldError(state, 'disk', 'target-free');
  state.disk['min-free'] = '20GB';
  fieldError(state, 'disk', 'target-free');
  state.disk['target-free'] = 'garbage';
  fieldError(state, 'disk', 'target-free');
  state.disk['target-free'] = '40GB';
  assert.match(generateFullConfig(state), /target-free = "40GB"/u);
});

test('Docker and Tart errors identify editable fields', () => {
  const state = base();
  state.scaleSets[0].docker['workspace-root'] = '/';
  fieldError(state, 'docker', 'workspace-root');
  state.scaleSets[0].docker['workspace-root'] = '';
  state.scaleSets[0].docker.platform = 'linux';
  fieldError(state, 'docker', 'platform');
  state.scaleSets[0].docker.platform = '';
  state.scaleSets[0].docker['cache-volumes'] = 'gradle:relative/path';
  fieldError(state, 'docker', 'cache-volumes');
  state.scaleSets[0].docker['cache-volumes'] = '';
  state.scaleSets[0].docker.cache = 'gradle | /home/runner/.gradle | 10% | warn';
  fieldError(state, 'docker', 'cache');
  state.scaleSets[0].set.provider = 'tart';
  state.scaleSets[0].set['max-runners'] = '2';
  state.scaleSets[0].tart.dns = 'not-an-ip';
  fieldError(state, 'tart', 'dns');
  state.scaleSets[0].tart.dns = '999.999.999.999';
  fieldError(state, 'tart', 'dns');
  state.scaleSets[0].tart.dns = '';
  state.scaleSets[0].tart['net-bridged'] = '-bad interface';
  fieldError(state, 'tart', 'net-bridged');
});

test('retry and generic field failures identify their form inputs', () => {
  const state = base();
  state.scaleSets[0].retry = { initial: '2m', max: '1m' };
  fieldError(state, 'retry', 'max');
  state.scaleSets[0].retry = {};
  state.global.concurrent = '-1';
  fieldError(state, 'global', 'concurrent');
  state.global.concurrent = '';
  state.scaleSets[0].set.name = 'bad\nname';
  fieldError(state, 'set', 'name');
});

test('an error in the second scale set points to its own input', () => {
  const state = base();
  const second = baseSet();
  second.set.name = 'macos-runners';
  second.docker.platform = 'linux';
  state.scaleSets.push(second);
  fieldError(state, 'docker', 'platform', 1);
});
