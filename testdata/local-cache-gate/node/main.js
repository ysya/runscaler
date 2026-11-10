require('./probe')(false).catch(() => { console.error('Local cache runtime gate failed before cache operations.'); process.exitCode = 1; });
