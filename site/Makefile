# Rebuild the website docs from docs/ on main, then check the result.
#   make            sync docs from origin/main and build
#   make og         render assets/og.png
#   make check      serve the site and check pages, links, redirects and meta tags
#   DOCS_REF=v0.2.0 make   build from a tag or another ref

.PHONY: build og check

build: node_modules
	npm run build

og: node_modules
	npm run og

check: node_modules
	npm run check

node_modules: package-lock.json
	npm ci
	@touch node_modules
