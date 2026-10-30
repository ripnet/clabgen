"""MkDocs hooks: the README is the home page (without its github-only
parts), light/dark screenshots and UI/YAML tabs written so GitHub shows
them too become Material's, and links that leave docs/ (examples, the
Python package, the license) point at GitHub."""

import re
import textwrap
from pathlib import Path

REPO = "https://github.com/ripnet/clabgen/blob/main/"
ROOT = Path(__file__).resolve().parents[2]


# A screenshot in a light and a dark version, as GitHub shows it (by the
# reader's theme):
#   <picture>
#     <source media="(prefers-color-scheme: dark)" srcset="x-dark.png">
#     <img alt="…" src="x.png">
#   </picture>
PICTURE = re.compile(
    r'<picture>\s*<source media="\(prefers-color-scheme: dark\)" srcset="([^"]+)">\s*'
    r'<img alt="([^"]*)" src="([^"]+)">\s*</picture>'
)


def light_dark(markdown):
    """Pictures as the two images Material shows by the site's palette (its
    light/dark toggle included, which a media query wouldn't follow)."""
    parts = re.split(r"(^```.*?^```)", markdown, flags=re.S | re.M)  # not in code blocks
    return "".join(p if i % 2 else PICTURE.sub(lambda m: f"![{m[2]}]({m[3]}#only-light)![{m[2]}]({m[1]}#only-dark)", p)
                   for i, p in enumerate(parts))


# Tabs, as GitHub shows them (one after the other):
#   <!-- tab: UI -->
#   …
#   <!-- tab: YAML -->
#   …
#   <!-- /tabs -->
TABS = re.compile(r"^<!-- tab: .+? -->\n.*?^<!-- /tabs -->[ \t]*\n?", re.S | re.M)


def tabs(markdown):
    """Tab groups as Material's content tabs (pymdownx.tabbed). Examples of
    the syntax go in ~~~ blocks, which are left alone."""

    def group(m):
        pieces = re.split(r"^<!-- tab: (.+?) -->\n", m[0].replace("<!-- /tabs -->", ""), flags=re.M)
        return "\n".join(f'=== "{label}"\n\n' + textwrap.indent(body.strip("\n"), "    ") + "\n"
                         for label, body in zip(pieces[1::2], pieces[2::2])) + "\n"

    parts = re.split(r"(^~~~.*?^~~~)", markdown, flags=re.S | re.M)
    return "".join(p if i % 2 else TABS.sub(group, p) for i, p in enumerate(parts))


def on_page_markdown(markdown, page, config, files):
    if page.file.src_uri == "index.md":
        # The README, with its docs/ links made relative to the site.
        markdown = (ROOT / "README.md").read_text()
        # What only makes sense on GitHub (e.g. a link to this site).
        markdown = re.sub(r"<!-- github-only -->.*?<!-- /github-only -->\n?", "", markdown, flags=re.S)
        markdown = light_dark(markdown)
        markdown = re.sub(r"\]\(docs/", "](", markdown)
        markdown = re.sub(r"\]\((?!https?:|#|[a-z-]+\.md|images/)([^)]+)\)", lambda m: f"]({REPO}{m.group(1)})", markdown)
        return markdown
    markdown = tabs(light_dark(markdown))
    # ../examples, ../python, ../.github, …: on GitHub.
    return re.sub(r"\]\(\.\./([^)]+)\)", lambda m: f"]({REPO}{m.group(1)})", markdown)
