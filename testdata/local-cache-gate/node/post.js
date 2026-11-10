require('./probe')(true).catch(() => { console.error('Local cache post-step gate failed.'); process.exitCode = 1; });
