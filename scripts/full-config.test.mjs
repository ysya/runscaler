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
