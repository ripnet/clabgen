"""MkDocs hooks: the README is the home page, and links that leave docs/
(examples, the Python package, the license) point at GitHub."""

import re
from pathlib import Path

REPO = "https://github.com/ripnet/clabgen/blob/main/"
ROOT = Path(__file__).resolve().parents[2]


def on_page_markdown(markdown, page, config, files):
    if page.file.src_uri == "index.md":
        # The README, with its docs/ links made relative to the site.
        markdown = (ROOT / "README.md").read_text()
        markdown = re.sub(r"\]\(docs/", "](", markdown)
        markdown = re.sub(r"\]\((?!https?:|#|[a-z-]+\.md|images/)([^)]+)\)", lambda m: f"]({REPO}{m.group(1)})", markdown)
        return markdown
    # ../examples, ../python, ../.github, …: on GitHub.
    return re.sub(r"\]\(\.\./([^)]+)\)", lambda m: f"]({REPO}{m.group(1)})", markdown)
