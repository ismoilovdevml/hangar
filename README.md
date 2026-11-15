# Hangar website

The GitHub Pages site of [Hangar](https://github.com/ismoilovdevml/hangar), served from this `gh-pages` branch.

- `index.html`: the landing page, with the live demo in a browser frame
- `docs.html`: the documentation; every page lives in `assets/docs.js` and is opened by its hash (`docs.html#quick-start`)
- `demo/`: the live demo, the web UI built with sample data (`npm run build:demo` in `web/` on `main`)
- `404.html`: sends reloaded demo pages (`/hangar/demo/repositories`) to the demo's hash form
- `assets/`: styles, scripts and images; `assets/img/ui/` holds the screenshots of the UI guides

Plain static files, no build step. The demo is built for the `/hangar/demo/` path, so preview from a parent directory: `mkdir -p /tmp/site && ln -sfn "$PWD" /tmp/site/hangar && python3 -m http.server -d /tmp/site 8000`, then open `http://localhost:8000/hangar/`.

## Refresh the demo and the screenshots

From a checkout of `main`, with `SITE` pointing at this branch's working tree:

```sh
cd web
npm ci
npm run build:demo && rm -rf "$SITE/demo" && cp -R dist-demo "$SITE/demo"
npm run site:screenshots -- "$SITE"
```

The full guides live on the `main` branch in [`docs/`](https://github.com/ismoilovdevml/hangar/tree/main/docs). Keep this site in line with them when a release changes behaviour.
