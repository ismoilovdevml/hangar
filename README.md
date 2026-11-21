# Hangar website

The GitHub Pages site of [Hangar](https://github.com/ismoilovdevml/hangar), the self-hosted package registry, served from this `gh-pages` branch at <https://ismoilovdevml.github.io/hangar/>.

- `index.html`: the landing page, with the live demo in a browser frame (hand-written)
- `docs/`: the documentation, one page per markdown file, **generated** by `site/` from `docs/` on `main`; do not edit it by hand
- `docs.html`: generated; sends the old one-page links (`docs.html#quick-start`) to the new pages (map in `site/redirects.json`)
- `sitemap.xml`, `robots.txt`, `assets/search-index.json`, `assets/fonts/`: generated with the docs
- `assets/og.png`: the link preview image, drawn as SVG and rasterised by `site/og/render.mjs` (resvg, no browser)
- `demo/`: the live demo, the web UI built with sample data (`npm run build:demo` in `web/` on `main`)
- `404.html`: sends reloaded demo pages (`/hangar/demo/repositories`) to the demo's hash form
- `assets/`: styles and scripts (`site.css`/`site.js` for the landing page, `docs.css`/`docs-ui.js` for the docs) and images; `assets/img/ui/` holds the screenshots of the UI guides
- `site/`: the docs generator (Node, markdown-it, highlight.js) and the pages that exist only here: `site/content/` (introduction, FAQ, the web UI walkthroughs); `site/nav.mjs` is the sidebar

## Rebuild the docs from main

Needs Node 20+ and git. From a checkout of this branch:

```sh
git fetch origin main
cd site
npm ci
npm run build      # copies docs/ of origin/main into site/.cache, writes docs/, docs.html, sitemap.xml, ...
npm run check      # checks every page's links, #fragments, meta tags and headings, the redirects and the sitemap
```

`make` in `site/` does the same (`make`, `make check`, `make og`). Build from a tag with `DOCS_REF=v0.2.0 npm run build`, or from a working tree with `DOCS_DIR=/path/to/hangar/docs npm run build`. The build lists every warning (a link to a missing page, a markdown file that is not in `site/nav.mjs`); `STRICT=1` makes them fail it.

A new markdown file on `main` is built automatically and listed under "More" until it gets a place in `site/nav.mjs`. Re-render the preview image after changing `site/og/render.mjs`: `npm run og`. Neither the checks nor the image need a browser; the checks read the files, so runtime errors in the browser console are outside what they see.

## Preview

The demo is built for the `/hangar/demo/` path, so preview from a parent directory: `mkdir -p /tmp/site && ln -sfn "$PWD" /tmp/site/hangar && python3 -m http.server -d /tmp/site 8000`, then open `http://localhost:8000/hangar/`.

## Refresh the demo and the screenshots

From a checkout of `main`, with `SITE` pointing at this branch's working tree:

```sh
cd web
npm ci
npm run build:demo && rm -rf "$SITE/demo" && cp -R dist-demo "$SITE/demo"
npm run site:screenshots -- "$SITE"
```

## On a release

Bump the version in `index.html` (the Docker Compose, Docker and Kubernetes tabs and `softwareVersion` in the JSON-LD), then rebuild the docs from the release tag.
