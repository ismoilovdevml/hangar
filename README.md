# Hangar website

The GitHub Pages site of [Hangar](https://github.com/ismoilovdevml/hangar), served from this `gh-pages` branch.

- `index.html`: the landing page
- `docs.html`: the documentation; every page lives in `assets/docs.js` and is opened by its hash (`docs.html#quick-start`)
- `assets/`: styles, scripts and screenshots

Plain static files, no build step. Preview locally with `python3 -m http.server` and open `http://localhost:8000`.

The full guides live on the `main` branch in [`docs/`](https://github.com/ismoilovdevml/hangar/tree/main/docs). Keep this site in line with them when a release changes behaviour.
