import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { generateConfig, quoteToml } from '../site/config.mjs';

const base = {
  provider: 'docker',
  url: 'https://github.com/example-org',
  name: 'linux-runners',
  maxRunners: 10,
  dind: true,
  runnerImage: 'ghcr.io/actions/actions-runner:latest',
  auth: 'pat',
};

test('Docker PAT config keeps the credential empty for local entry', () => {
  const toml = generateConfig(base);
  assert.match(toml, /token = ""/);
  assert.match(toml, /provider = "docker"/);
  assert.match(toml, /\[docker\]\ndind = true/);
  assert.match(toml, /max-runners = 10/);
  assert.doesNotMatch(toml, /github-app\./);
});

test('Tart config caps capacity and omits Docker options', () => {
  const toml = generateConfig({ ...base, provider: 'tart', maxRunners: 2, runnerImage: 'ghcr.io/cirruslabs/macos-tahoe-xcode:latest' });
  assert.match(toml, /provider = "tart"/);
  assert.match(toml, /max-runners = 2/);
  assert.doesNotMatch(toml, /\[docker\]/);
  assert.throws(() => generateConfig({ ...base, provider: 'tart', maxRunners: 3 }), /at most 2/);
});

test('GitHub App config records only a local key-file reference', () => {
  const toml = generateConfig({ ...base, auth: 'app', clientID: 'Iv1.example', installationID: 123, privateKeyFile: '/etc/runner/app.pem' });
  assert.match(toml, /github-app\.client-id = "Iv1\.example"/);
  assert.match(toml, /github-app\.installation-id = 123/);
  assert.match(toml, /github-app\.private-key-file = "\/etc\/runner\/app\.pem"/);
  assert.doesNotMatch(toml, /^token =/m);
});

test('TOML quoting keeps quotes and backslashes inside string values', () => {
  assert.equal(quoteToml('a"\\b'), '"a\\"\\\\b"');
  assert.throws(() => quoteToml('name"\nmax-runners = 999'), /control character/);
});

test('invalid public inputs are rejected', () => {
  assert.throws(() => generateConfig({ ...base, url: 'http://github.com/example-org' }), /HTTPS/);
  assert.throws(() => generateConfig({ ...base, name: ' ' }), /name/);
  assert.throws(() => generateConfig({ ...base, maxRunners: 0 }), /positive/);
  assert.throws(() => generateConfig({ ...base, auth: 'app', clientID: 'Iv1.example', installationID: 123, privateKeyFile: 'relative.pem' }), /absolute/);
  assert.throws(() => generateConfig({ ...base, runnerImage: '' }), /runner image/);
  for (const url of ['https://github.com/enterprises/example', 'https://github.com/%65nterprises/example']) {
    assert.throws(() => generateConfig({ ...base, url, auth: 'app' }), /unavailable for enterprise-level runners/);
  }
});

for (const [file, changes] of [
  ['docker-pat.toml', {}],
  ['docker-app.toml', { auth: 'app', clientID: 'Iv1.example', installationID: 123, privateKeyFile: '/etc/runner/app.pem' }],
  ['tart-pat.toml', { provider: 'tart', maxRunners: 2, runnerImage: 'ghcr.io/cirruslabs/macos-tahoe-xcode:latest' }],
  ['tart-app.toml', { provider: 'tart', maxRunners: 2, runnerImage: 'ghcr.io/cirruslabs/macos-tahoe-xcode:latest', auth: 'app', clientID: 'Iv1.example', installationID: 123, privateKeyFile: '/Users/test/runner-app.pem' }],
]) {
  test(`${file} matches the parser fixture`, () => {
    const expected = readFileSync(new URL(`../testdata/site/${file}`, import.meta.url), 'utf8');
    assert.equal(generateConfig({ ...base, name: 'example-runners', ...changes }), expected);
  });
}
