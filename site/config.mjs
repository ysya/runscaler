export function quoteToml(value) {
  const text = String(value);
  if (/[\u0000-\u001f\u007f]/u.test(text)) {
    throw new Error('Values cannot contain control characters.');
  }
  for (let i = 0; i < text.length; i += 1) {
    const code = text.charCodeAt(i);
    if (code >= 0xd800 && code <= 0xdbff) {
      const next = text.charCodeAt(i + 1);
      if (!(next >= 0xdc00 && next <= 0xdfff)) throw new Error('Invalid Unicode in value.');
      i += 1;
    } else if (code >= 0xdc00 && code <= 0xdfff) {
      throw new Error('Invalid Unicode in value.');
    }
  }
  return JSON.stringify(text);
}

export function generateConfig(input) {
  const provider = input.provider;
  if (provider !== 'docker' && provider !== 'tart') throw new Error('Choose Docker or Tart.');

  const url = String(input.url ?? '').trim();
  let parsed;
  try {
    parsed = new URL(url);
  } catch {
    throw new Error('Enter a valid HTTPS GitHub registration URL.');
  }
  if (parsed.protocol !== 'https:' || !parsed.hostname || parsed.username || parsed.password || parsed.pathname === '/' || parsed.search || parsed.hash) {
    throw new Error('Enter an HTTPS GitHub organization or repository URL without credentials, query, or fragment.');
  }

  const name = String(input.name ?? '').trim();
  if (!name) throw new Error('Enter a scale-set name.');
  const max = Number(input.maxRunners);
  if (!Number.isSafeInteger(max) || max < 1) throw new Error('Maximum runners must be a positive whole number.');
  if (provider === 'tart' && max > 2) throw new Error('Tart supports at most 2 concurrent VMs per host.');

  const image = String(input.runnerImage ?? '').trim();
  if (!image) throw new Error('Enter a runner image.');

  const lines = [
    '# runner configuration',
    `url = ${quoteToml(url)}`,
    `name = ${quoteToml(name)}`,
  ];
  if (input.auth === 'pat') {
    lines.push('# Add your PAT locally before validating.', 'token = ""');
  } else if (input.auth === 'app') {
    let path;
    try {
      path = decodeURIComponent(parsed.pathname).replace(/^\/+|\/+$/gu, '');
    } catch {
      throw new Error('Enter a valid HTTPS GitHub registration URL.');
    }
    if (path.startsWith('enterprises/')) throw new Error('GitHub App authentication is unavailable for enterprise-level runners; use a PAT.');
    const clientID = String(input.clientID ?? '').trim();
    const installationID = Number(input.installationID);
    const privateKeyFile = String(input.privateKeyFile ?? '').trim();
    if (!clientID) throw new Error('Enter the GitHub App client ID.');
    if (!Number.isSafeInteger(installationID) || installationID < 1) throw new Error('Enter a positive GitHub App installation ID.');
    if (!privateKeyFile.startsWith('/')) throw new Error('Enter an absolute private-key file path.');
    lines.push(
      `github-app.client-id = ${quoteToml(clientID)}`,
      `github-app.installation-id = ${installationID}`,
      `github-app.private-key-file = ${quoteToml(privateKeyFile)}`,
    );
  } else {
    throw new Error('Choose PAT or GitHub App authentication.');
  }

  lines.push(
    `max-runners = ${max}`,
    'log-level = "info"',
    'log-format = "text"',
    'health-port = 8080',
    `provider = ${quoteToml(provider)}`,
    `runner-image = ${quoteToml(image)}`,
  );
  if (provider === 'docker') {
    lines.push('', '[docker]', `dind = ${input.dind === false ? 'false' : 'true'}`);
  }
  return `${lines.join('\n')}\n`;
}
