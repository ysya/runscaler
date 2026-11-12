#!/usr/bin/env python3
"""Check the dependency-free GitHub Pages source without a browser toolchain."""

from html.parser import HTMLParser
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
SITE = ROOT / "site"


class Page(HTMLParser):
    def __init__(self):
        super().__init__()
        self.ids = set()
        self.duplicates = set()
        self.scripts = []
        self.styles = []
        self.inputs = []
        self.lang = None
        self.body_lang = None

    def handle_starttag(self, tag, attrs):
        values = dict(attrs)
        ident = values.get("id")
        if ident:
            if ident in self.ids:
                self.duplicates.add(ident)
            self.ids.add(ident)
        if tag == "html":
            self.lang = values.get("lang")
        elif tag == "body":
            self.body_lang = values.get("data-lang")
        elif tag == "script":
            self.scripts.append(values)
        elif tag == "link" and values.get("rel") == "stylesheet":
            self.styles.append(values.get("href"))
        elif tag == "input":
            self.inputs.append(values)


required_ids = {
    "config-form", "registration-url", "scale-name", "max-runners",
    "runner-image", "dind", "app-fields", "client-id", "installation-id",
    "key-file", "config-output", "copy-config", "form-message",
    "copy-status", "credential-note", "docker-steps", "tart-steps",
    "workflow-output", "configure", "how",
}

for filename, html_lang, body_lang in (
    ("index.html", "en", "en"),
    ("zh-TW.html", "zh-Hant", "zh-TW"),
):
    source = (SITE / filename).read_text()
    page = Page()
    page.feed(source)
    assert page.lang == html_lang and page.body_lang == body_lang, filename
    assert not page.duplicates, (filename, page.duplicates)
    assert required_ids <= page.ids, (filename, required_ids - page.ids)
    assert page.scripts == [{"type": "module", "src": "./app.mjs"}], filename
    assert page.styles == ["./style.css"], filename
    assert all(field.get("type") != "password" for field in page.inputs), filename
    assert "Content-Security-Policy" in source and "form-action 'none'" in source, filename
    assert "https://github.com/ysya/runscaler" in source, filename

for filename in ("app.mjs", "config.mjs"):
    source = (SITE / filename).read_text()
    for banned in ("localStorage", "sessionStorage", "fetch(", "XMLHttpRequest", "sendBeacon"):
        assert banned not in source, (filename, banned)

print("Static site checks passed.")
