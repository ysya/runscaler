#!/usr/bin/env python3
"""Check the dependency-free GitHub Pages source without a browser toolchain."""

from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import unquote, urljoin, urlsplit
import xml.etree.ElementTree as ET


ROOT = Path(__file__).resolve().parent.parent
SITE = ROOT / "site"
ORIGIN = "https://runscaler.ysya.dev"


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
        self.links = []
        self.meta = {}
        self.title = ""
        self.in_title = False

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
        if tag in ("a", "link") and "href" in values:
            self.links.append(values)
        if tag == "meta":
            self.meta[values.get("name", values.get("property"))] = values.get("content")
        if tag == "title":
            self.in_title = True

    def handle_data(self, data):
        if self.in_title:
            self.title += data

    def handle_endtag(self, tag):
        if tag == "title":
            self.in_title = False


def public_url(filename):
    return ORIGIN + "/" + filename.removesuffix("index.html")


pages = {}


for filename, html_lang, body_lang, required_ids, scripts, styles in (
    ("index.html", "en", "en", {"configure", "how"}, [], ["./style.css"]),
    ("zh-TW.html", "zh-Hant", "zh-TW", {"configure", "how"}, [], ["./style.css"]),
    ("configure/index.html", "en", "en", {"builder-form", "scale-sets", "scale-list", "native-cache-fields", "disk-fields", "global-fields", "config-output", "copy-config", "form-message", "copy-status"}, [{"type": "module", "src": "./app.mjs"}], ["../style.css", "./style.css"]),
    ("configure/zh-TW.html", "zh-Hant", "zh-TW", {"builder-form", "scale-sets", "scale-list", "native-cache-fields", "disk-fields", "global-fields", "config-output", "copy-config", "form-message", "copy-status"}, [{"type": "module", "src": "./app.mjs"}], ["../style.css", "./style.css"]),
    ("guide/index.html", "en", "en", {"docker", "macos", "compare", "faq"}, [], ["../style.css"]),
    ("guide/zh-TW.html", "zh-Hant", "zh-TW", {"docker", "macos", "compare", "faq"}, [], ["../style.css"]),
):
    source = (SITE / filename).read_text()
    page = Page()
    page.feed(source)
    pages[filename] = page
    assert page.lang == html_lang and page.body_lang == body_lang, filename
    assert not page.duplicates, (filename, page.duplicates)
    assert required_ids <= page.ids, (filename, required_ids - page.ids)
    assert page.scripts == scripts, filename
    assert page.styles == styles, filename
    assert all(field.get("type") != "password" for field in page.inputs), filename
    assert "Content-Security-Policy" in source and "form-action 'none'" in source, filename
    assert "https://github.com/ysya/runscaler" in source, filename
    if filename.startswith("configure/"):
        assert "config-output" in page.ids and "builder-form" in page.ids, filename
    else:
        assert "config-output" not in page.ids and "config-form" not in page.ids, filename
        assert "configure/" in source, filename

    canonical = [link["href"] for link in page.links if link.get("rel") == "canonical"]
    assert canonical == [public_url(filename)], (filename, "canonical", canonical)
    directory = filename.rpartition("/")[0]
    prefix = directory + "/" if directory else ""
    english = public_url(prefix + "index.html")
    chinese = public_url(prefix + "zh-TW.html")
    alternates = [link for link in page.links if link.get("rel") == "alternate" and link.get("hreflang")]
    assert len(alternates) == 3, (filename, "language alternatives")
    assert {link["hreflang"]: link["href"] for link in alternates} == {
        "en": english, "zh-Hant": chinese, "x-default": english,
    }, (filename, "language alternatives")
    assert page.title.strip() and page.meta.get("description"), (filename, "title/description")
    assert page.meta.get("og:title") == page.title, (filename, "Open Graph title")
    assert page.meta.get("og:description") == page.meta["description"], (filename, "Open Graph description")
    assert page.meta.get("og:url") == public_url(filename), (filename, "Open Graph URL")
    assert any(link.get("rel") == "describedby" and link["href"] == "/llms.txt" for link in page.links), (filename, "documentation index")

assert set(pages) == {path.relative_to(SITE).as_posix() for path in SITE.rglob("*.html")}, "Unchecked HTML route"

for filename, page in pages.items():
    for link in page.links:
        target = urlsplit(urljoin(public_url(filename), link["href"]))
        if target.scheme not in ("http", "https") or target.netloc != urlsplit(ORIGIN).netloc:
            continue
        relative = unquote(target.path).lstrip("/")
        path = SITE / relative
        if path.is_dir():
            path /= "index.html"
        assert path.is_file(), (filename, "broken link", link["href"])
        if target.fragment and path.suffix == ".html":
            target_page = pages[str(path.relative_to(SITE))]
            assert unquote(target.fragment) in target_page.ids, (filename, "broken anchor", link["href"])

sitemap = ET.parse(SITE / "sitemap.xml")
locations = [node.text for node in sitemap.findall("{http://www.sitemaps.org/schemas/sitemap/0.9}url/{http://www.sitemaps.org/schemas/sitemap/0.9}loc")]
assert len(locations) == len(set(locations)), "Duplicate sitemap URL"
assert set(locations) == {public_url(filename) for filename in pages}, "Sitemap does not match HTML routes"

robots = (SITE / "robots.txt").read_text()
assert "User-agent: *\nAllow: /" in robots, "Crawler access"
assert "Disallow:" not in robots, "Unexpected crawler restriction"
assert f"Sitemap: {ORIGIN}/sitemap.xml" in robots, "Sitemap discovery"
llms = (SITE / "llms.txt").read_text()
assert llms.startswith("# Runscaler\n"), "Documentation index identity"
assert f"{ORIGIN}/guide/" in llms, "Selection guide discovery"
assert "raw.githubusercontent.com/ysya/runscaler/main/docs/" in llms, "Markdown documentation links"

for filename in ("configure/app.mjs", "configure/config.mjs", "configure/schema.mjs", "configure/toml.mjs"):
    source = (SITE / filename).read_text()
    for banned in ("localStorage", "sessionStorage", "fetch(", "XMLHttpRequest", "sendBeacon"):
        assert banned not in source, (filename, banned)

print("Static site checks passed.")
