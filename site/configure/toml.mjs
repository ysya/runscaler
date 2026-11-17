export function quoteToml(value, lang = 'en') {
  const text = String(value);
  if (/[\u0000-\u001f\u007f]/u.test(text)) throw new Error(lang === 'ja' ? '値に制御文字を含めることはできません。' : 'Values cannot contain control characters.');
  for (let i = 0; i < text.length; i += 1) {
    const code = text.charCodeAt(i);
    if (code >= 0xd800 && code <= 0xdbff) {
      const next = text.charCodeAt(i + 1);
      if (!(next >= 0xdc00 && next <= 0xdfff)) throw new Error(lang === 'ja' ? '値に無効な Unicode が含まれています。' : 'Invalid Unicode in value.');
      i += 1;
    } else if (code >= 0xdc00 && code <= 0xdfff) {
      throw new Error(lang === 'ja' ? '値に無効な Unicode が含まれています。' : 'Invalid Unicode in value.');
    }
  }
  return JSON.stringify(text);
}
