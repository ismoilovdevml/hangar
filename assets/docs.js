/* Hangar docs: content, hash router, search, table of contents, theme. No dependencies.
   Edit the three values below once and every page updates. */
var SITE = {
  owner: 'ismoilovdevml',               // GitHub user or organisation that hosts the repo
  host: 'hangar.example.com',           // example address used in snippets
  repoPath: '/repository/'              // URL prefix for package repositories
};

(function () {
  'use strict';
  var H = SITE.host, URL = 'https://' + SITE.host, R = URL + SITE.repoPath, GH = 'https://github.com/' + SITE.owner + '/hangar', IMG = 'ghcr.io/' + SITE.owner + '/hangar';
  var DOC = GH + '/blob/main/', INSTALL = GH + '/releases/latest/download/install.sh';

  /* ---------- small content helpers ---------- */
  function esc(s) { return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }
  function hl(lang, src) {
    return src.split('\n').map(function (line) {
      var e = esc(line);
      if (/^\s*(#|\/\/)/.test(line)) return '<span class="c">' + e + '</span>';
      if (lang === 'shell' && /^\$ /.test(line)) e = '<span class="p">$ </span>' + e.slice(2);
      if (lang === 'yaml' || lang === 'toml') e = e.replace(/^(\s*-?\s*)([\w.\-]+)(:|\s=)/, '$1<span class="k">$2</span>$3');
      if (lang === 'json') e = e.replace(/(&quot;|")([\w.\-@\/]+)\1(\s*:)/g, '<span class="k">"$2"</span>$3');
      e = e.replace(/(\s#\s.*)$/, '<span class="c">$1</span>');
      return e;
    }).join('\n');
  }
  function code(lang, src, title) {
    return '<div class="code"><div class="code-head"><span>' + (title || lang) + '</span><button class="code-copy" type="button">Copy</button></div><pre data-lang="' + lang + '">' + hl(lang, src.replace(/^\n|\n\s*$/g, '')) + '</pre></div>';
  }
  function tabs(list) {
    var id = 't' + Math.random().toString(36).slice(2, 7);
    return '<div class="tabs"><div role="tablist">' + list.map(function (t, i) {
      return '<button role="tab" type="button" id="' + id + i + '" aria-selected="' + (i === 0) + '" aria-controls="' + id + 'p' + i + '">' + t[0] + '</button>';
    }).join('') + '</div>' + list.map(function (t, i) {
      return '<div role="tabpanel" id="' + id + 'p' + i + '" aria-labelledby="' + id + i + '"' + (i ? ' hidden' : '') + '>' + t[1] + '</div>';
    }).join('') + '</div>';
  }
  function call(kind, title, body) { return '<div class="call ' + (kind || '') + '"><p><b>' + title + '</b> ' + body + '</p></div>'; }
  function table(head, rows) {
    return '<div class="tbl-wrap"><table class="tbl"><thead><tr>' + head.map(function (h) { return '<th>' + h + '</th>'; }).join('') + '</tr></thead><tbody>' +
      rows.map(function (r) { return '<tr>' + r.map(function (c) { return '<td>' + c + '</td>'; }).join('') + '</tr>'; }).join('') + '</tbody></table></div>';
  }
  function cards(list) { return '<div class="cards">' + list.map(function (c) { return '<a class="card" href="#' + c[0] + '"><b>' + c[1] + '</b><span>' + c[2] + '</span></a>'; }).join('') + '</div>'; }
  function steps(list) { return '<ol class="steps">' + list.map(function (s) { return '<li><b>' + s[0] + '</b>' + s[1] + '</li>'; }).join('') + '</ol>'; }
  function ep(verb, path, desc) { return '<div class="endpoint"><span class="verb ' + verb.toLowerCase().slice(0, 4).replace('dele', 'del') + '">' + verb + '</span>' + path + '</div><p>' + desc + '</p>'; }
  function guide(file, label) { return '<a href="' + DOC + file + '">' + (label || file.split('/').pop()) + '</a>'; }
  function fmtPage(o) {
    var n = o.names || {};
    var rows = o.rows || [
      ['<code>' + (n.proxy || o.slug + '-proxy') + '</code>', 'proxy', 'Caches <a href="' + o.upstreamUrl + '">' + o.upstream + '</a>. The first request fetches a file; later requests are served from Hangar.'],
      ['<code>' + (n.hosted || o.slug + '-hosted') + '</code>', 'hosted', 'Stores the ' + o.what + ' your team publishes.'],
      ['<code>' + (n.group || o.slug + '-group') + '</code>', 'group', o.groupText || 'One URL that tries its members in order: hosted first, then the proxy. Point your tools here.']
    ];
    return '<h2>Create the repositories</h2><p>Open <strong>Repositories → New repository</strong> and pick <strong>' + o.name + '</strong>, or create them with the <a href="#api-repositories">API</a>. A typical setup uses three:</p>' +
      table(['Name', 'Type', 'What it does'], rows) +
      '<h2>Point your client at Hangar</h2>' + o.client +
      '<h2>Publish</h2>' + o.publish +
      (o.notes ? '<h2>Good to know</h2>' + o.notes : '');
  }

  /* ---------- the pages ---------- */
  var NAV = [
    ['Getting started', [
      ['introduction', 'Introduction'], ['installation', 'Installation'], ['quick-start', 'Quick start'], ['concepts', 'Core concepts']]],
    ['Package formats', [
      ['docker', 'Docker & OCI'], ['npm', 'npm'], ['nuget', 'NuGet'], ['maven', 'Maven & Gradle'], ['pypi', 'PyPI'], ['go', 'Go modules'], ['helm', 'Helm charts'],
      ['cargo', 'Cargo'], ['apt', 'APT'], ['rpm', 'RPM'], ['raw', 'Raw files']]],
    ['Using Hangar', [
      ['proxies', 'Proxies & caching'], ['vulnerabilities', 'Vulnerability scanning'], ['routing', 'Routing rules'], ['migrate', 'Move to Hangar'], ['status', 'Health & status'], ['ci', 'CI/CD pipelines']]],
    ['Running Hangar', [
      ['configuration', 'Configuration'], ['authentication', 'Sign-in & tokens'], ['storage', 'Storage & cleanup'], ['cluster', 'Clustering'], ['backups', 'Backups & restore'], ['upgrading', 'Upgrading']]],
    ['API reference', [
      ['api', 'Overview'], ['api-repositories', 'Repositories'], ['api-packages', 'Components & search'], ['api-vulnerabilities', 'Vulnerabilities'], ['api-tokens', 'Tokens & users'], ['api-system', 'System']]],
    ['Help', [
      ['troubleshooting', 'Troubleshooting'], ['faq', 'FAQ']]]
  ];

  var P = {};

  P.introduction = { src: 'README.md', title: 'Introduction', lead: 'Hangar is a self-hosted package registry. It keeps every package your builds need in one place: what you publish, and cached copies of what you download.', body:
    fig('overview', 'The Overview page: today in one sentence, and only the things that need you.') +
    '<h2>Why teams use it</h2><ul>' +
    '<li><strong>Builds stop depending on the internet.</strong> Once a package is cached, a public registry that is down no longer breaks CI: proxy repositories keep serving what they have.</li>' +
    '<li><strong>One registry for eleven formats.</strong> Docker/OCI, npm, Maven, PyPI, NuGet, Go modules, Helm, Cargo, APT, RPM and raw files share one sign-in, one set of roles and one audit log.</li>' +
    '<li><strong>You see what is risky.</strong> Packages are checked against <a href="https://osv.dev">OSV</a> advisories and container images with Trivy. The fix plan shows what to upgrade first, and a repository can quarantine versions with critical findings.</li>' +
    '<li><strong>It is small.</strong> One Go binary with the web UI built in, PostgreSQL for metadata, and package files on a filesystem or in S3. No JVM.</li></ul>' +
    '<h2>Where to go next</h2>' + cards([
      ['installation', 'Install', 'Install script (Docker Compose), Docker, Helm or a plain binary.'],
      ['quick-start', 'Quick start', 'From zero to a cached npm install in a few minutes.'],
      ['concepts', 'Core concepts', 'Hosted, proxy and group repositories explained.'],
      ['configuration', 'Configuration', 'hangar.yaml, HANGAR_* variables and secret files.']]) +
    call('', 'Source on GitHub.', 'The code, issues, changelog and roadmap are in <a href="' + GH + '">' + SITE.owner + '/hangar</a>. Bug reports and pull requests are welcome.') };

  P.installation = { src: 'docs/deploy/README.md', title: 'Installation', lead: 'Hangar needs PostgreSQL 16 or newer and storage for package files. The install script sets up both on one Linux host.', body:
    '<h2>Requirements</h2>' + table(['', 'Small: up to 50 users, 10 CI runners', 'Medium: up to 500 users, 100 CI runners'], [
      ['Hangar', '1 node: 2 vCPU, 4 GiB', '2 nodes: 4 vCPU, 8 GiB each'],
      ['PostgreSQL 16+', '2 vCPU, 4 GiB, 20 GiB SSD', '4 vCPU, 16 GiB, 100 GiB SSD'],
      ['Blob storage', '200 GiB', '1 to 5 TiB, S3']]) +
    '<p>These are starting points from the ' + guide('docs/deploy/requirements.md', 'sizing guide') + '; measure your own workload.</p>' +
    '<h2>Choose a method</h2>' + tabs([
      ['Script', code('shell', '$ curl -fsSL ' + INSTALL + ' | sudo bash') +
        '<p>For a Linux host with Docker Engine and the Compose plugin (2.20+). It asks for a data directory (default <code>/srv/hangar</code>) and an optional domain, generates every secret, starts Hangar and PostgreSQL with Docker Compose, and prints the URL, the user <code>admin</code> and a one-time password. Without a domain, Hangar is served at <code>http://&lt;host-ip&gt;:8080</code>. Re-run it to upgrade.</p>' +
        code('shell', '# Unattended, a pinned release, with container image scanning\n$ curl -fsSL ' + INSTALL + ' \\\n    | sudo bash -s -- --data-dir /srv/hangar --version vX.Y.Z --with-trivy --yes') +
        table(['Flag', 'Meaning'], [
          ['<code>--data-dir DIR</code>', 'Where everything is stored (default <code>/srv/hangar</code>); required without a terminal.'],
          ['<code>--domain NAME</code>', 'Public host name. Without one Hangar is served at <code>http://&lt;host-ip&gt;:&lt;port&gt;</code>.'],
          ['<code>--tls caddy|proxy</code>', 'With a domain: <code>caddy</code> gets a certificate itself (ports 80 and 443); <code>proxy</code> runs behind your reverse proxy.'],
          ['<code>--port PORT</code>', 'Host port of Hangar\'s HTTP listener (default 8080).'],
          ['<code>--version TAG</code>', 'Release to install, for example <code>v0.1.8</code> (default: latest).'],
          ['<code>--with-trivy</code>', 'Also run a Trivy server and Hangar\'s scan adapter to scan Docker images.'],
          ['<code>--yes</code>', 'Do not ask; use flags and defaults.']]) +
        '<p>The data directory holds <code>compose.yaml</code>, <code>config/hangar.yaml</code>, <code>secrets/</code>, <code>postgres/</code>, <code>blobs/</code>, <code>nats/</code> and <code>backups/</code>. All flags: <code>--help</code> and ' + guide('docs/deploy/install-compose.md', 'install-compose.md') + '.</p>'],
      ['Docker', code('shell', '$ docker run -d --name hangar -p 8080:8080 \\\n    -v hangar-data:/var/lib/hangar \\\n    -e HANGAR_DATABASE_URL=\'postgres://hangar:secret@db.example.com:5432/hangar\' \\\n    -e HANGAR_DATABASE_MIGRATE_ON_START=true \\\n    ' + IMG + ':latest') +
        '<p>The image <code>' + IMG + '</code> is built for linux/amd64 and linux/arm64 and tagged with each release version (without the leading <code>v</code>), its minor version and <code>latest</code>. It needs a PostgreSQL 16+ database it can reach. For more than a trial also set <code>HANGAR_SERVER_BASE_URL</code>, <code>HANGAR_SECRET_KEY</code> and <code>HANGAR_DOCKER_TOKEN_KEY</code>; see <a href="#configuration">Configuration</a>.</p>'],
      ['Kubernetes', code('shell', '$ git clone ' + GH + '.git\n$ helm upgrade --install hangar hangar/deploy/helm/hangar \\\n    --namespace hangar --create-namespace \\\n    -f values-prod.yaml --set image.tag=<version> --wait') +
        '<p>The Helm chart lives in the repository at <code>deploy/helm/hangar</code>; it is not published to a chart registry. It runs two or more replicas against an external PostgreSQL 16+ and S3-compatible storage (or a ReadWriteMany volume), and can deploy a 3-node NATS cluster. Your values file needs at least <code>config.server.base_url</code> and the database. See ' + guide('docs/deploy/install-helm.md', 'install-helm.md') + '.</p>'],
      ['Binary', code('shell', '# Linux amd64 or arm64, web UI included\n$ curl -fLO ' + GH + '/releases/latest/download/hangar-linux-amd64\n$ curl -fLO ' + GH + '/releases/latest/download/SHA256SUMS\n$ sha256sum -c --ignore-missing SHA256SUMS\n$ sudo install -m 0755 hangar-linux-amd64 /usr/local/bin/hangar\n$ hangar serve --config /etc/hangar/hangar.yaml') +
        '<p>A systemd unit is in <code>deploy/systemd/hangar.service</code>. See ' + guide('docs/deploy/install-binary.md', 'install-binary.md') + '.</p>']]) +
    '<h2>First sign-in</h2><p>The install script prints the address, the user <code>admin</code> and a one-time password. At the first sign-in Hangar asks you to choose your own password; until then the account can do nothing else.</p>' +
    code('shell', '# Lost the one-time password before signing in?\n$ cd /srv/hangar && sudo docker compose exec hangar hangar admin reset-password --user admin') +
    '<p>With <code>docker run</code> or the binary, the first start on an empty database creates <code>admin</code> and prints its one-time password once to standard error (<code>docker logs hangar</code>, <code>journalctl -u hangar</code>).</p>' +
    call('warn', 'Then connect your company sign-in.', 'See <a href="#authentication">Sign-in & tokens</a>, so people use their own accounts.') +
    '<h2>Put it behind HTTPS</h2><p>Docker and Maven refuse plain-HTTP registries other than <code>localhost</code>, so serve Hangar over HTTPS. The install script does it for you with <code>--domain hangar.example.com --tls caddy</code> (Let\'s Encrypt, ports 80 and 443). Behind your own reverse proxy, use <code>--tls proxy --trusted-proxies &lt;proxy address&gt;</code>.</p>' +
    '<p>Whatever the proxy: set <code>server.base_url</code> to the public HTTPS URL, list the proxy in <code>server.trusted_proxies</code>, allow unlimited request bodies without buffering, and keep <code>/metrics</code> off the public host. For a binary install with Caddy on the same host:</p>' +
    code('caddy', H + ' {\n\t@metrics path /metrics\n\trespond @metrics 404\n\treverse_proxy 127.0.0.1:8080 {\n\t\tflush_interval -1\n\t}\n}', 'Caddyfile') +
    '<p>Traefik, Kubernetes ingress and a separate registry host: ' + guide('docs/deploy/reverse-proxy.md', 'reverse-proxy.md') + '.</p>' };

  P['quick-start'] = { src: 'docs/guides/getting-started.md', title: 'Quick start', lead: 'A few minutes from an empty server to an npm install that goes through Hangar.', body:
    steps([
      ['Start Hangar', code('shell', '$ curl -fsSL ' + INSTALL + ' | sudo bash') + '<p>It prints the URL, the user <code>admin</code> and a one-time password. Sign in and choose your own password. Other ways to run it: <a href="#installation">Installation</a>.</p>'],
      ['Add a proxy for npm', '<p>Open <strong>Repositories → New repository</strong>, pick <strong>npm</strong> and <strong>Proxy</strong>. Name it <code>npm-proxy</code>; the remote URL starts as <code>https://registry.npmjs.org</code>.</p>'],
      ['Create a token', '<p>Open <strong>Settings → My account → Personal access tokens → New token</strong>. Pick <strong>Read only</strong> and an expiry. Copy the secret now; it is shown once.</p>'],
      ['Point npm at Hangar', code('shell', '$ npm config set registry ' + R + 'npm-proxy/\n$ npm config set //' + H + SITE.repoPath + 'npm-proxy/:_authToken YOUR_TOKEN') + '<p>Use the address the installer printed in place of <code>' + URL + '</code>.</p>'],
      ['Install something', code('shell', '$ npm install react') + '<p>The first install fetches from registry.npmjs.org and keeps a copy. Open <strong>Packages</strong> in Hangar: <code>react</code> is there. It is scanned on arrival when the repository has <strong>Scan new packages for vulnerabilities</strong> on, otherwise by the hourly rescan.</p>' + fig('packages', 'Packages: what is cached and what is hosted, with downloads and security state.')]]) +
    call('ok', 'That is the whole loop.', 'Every other format works the same way: add a proxy (usually with a hosted repository and a group), point the client at it, build as usual.') +
    '<h2>Next</h2>' + cards([['docker', 'Docker images', 'Cache Docker Hub and push your own images.'], ['ci', 'CI/CD', 'Use Hangar from GitHub Actions, GitLab CI and Jenkins.'], ['vulnerabilities', 'Scanning', 'See what to upgrade first; quarantine critical findings.']]) };

  P.concepts = { src: 'docs/guides/README.md', title: 'Core concepts', lead: 'Four words cover most of Hangar: repository, component, blob store and policy.', body:
    '<h2>Repositories</h2><p>A repository holds packages of one format. Its name matches <code>^[a-z0-9][a-z0-9-]{1,62}$</code>. There are three types:</p>' +
    table(['Type', 'Holds', 'Use it for'], [
      ['<strong>Hosted</strong>', 'Packages you publish', 'Internal libraries, your own Docker images.'],
      ['<strong>Proxy</strong>', 'Cached copies from a remote', 'Docker Hub, npmjs, nuget.org, Maven Central, PyPI, crates.io, Debian and Rocky Linux mirrors.'],
      ['<strong>Group</strong>', 'Nothing itself; it combines others', 'One URL for clients. Members are read in order.']]) +
    '<p>Most groups can forward uploads to one hosted member (<code>write_member</code>). Maven and raw groups are read-only, and APT and RPM groups combine hosted members only. Every format except Docker is served at <code>' + R + '&lt;repository&gt;/</code>; Docker is served at <code>/v2/</code>, with the repository as the first part of the image name.</p>' +
    '<h2>Components, assets and blobs</h2><p>A <strong>component</strong> is one package version: an npm version, a Maven artifact version, a Docker tag, a raw file. Its files are <strong>assets</strong>. The bytes are <strong>blobs</strong>, stored once per blob store under their SHA-256 digest, so the same layer or jar in several repositories takes space once.</p>' +
    '<h2>Blob stores</h2><p>A blob store is where file bytes live: a local folder or an S3-compatible bucket. The store created on first start is <code>default</code>. Hosted and proxy repositories use one blob store each; groups store nothing. See <a href="#storage">Storage</a>.</p>' +
    '<h2>Policies</h2><ul>' +
    '<li><strong>Write policy</strong> of a hosted repository: <code>allow</code>, <code>allow_once</code> (a published version cannot change) or <code>deny</code>.</li>' +
    '<li><strong>Cleanup policies</strong> delete old or unused versions on a schedule, after a preview.</li>' +
    '<li><strong>Routing rules</strong> keep internal package names away from public proxies. See <a href="#routing">Routing rules</a>.</li>' +
    '<li><strong>Scanning settings</strong> per repository: scan on arrival, and quarantine of versions with critical findings. See <a href="#vulnerabilities">Vulnerability scanning</a>.</li></ul>' +
    '<h2>How a download flows</h2>' + steps([
      ['Ask', '<p>A client asks the group for <code>Serilog 2.10.0</code>.</p>'],
      ['Look', '<p>Hangar tries the members in order, hosted first, then proxies, skipping members the routing rules exclude for that name.</p>'],
      ['Fetch once', '<p>If only the proxy can answer and nothing is cached, Hangar downloads the file from the remote, checks it and stores it by digest.</p>'],
      ['Check', '<p>A new version is queued for a scan when the repository scans on arrival. A quarantined version is not served (403).</p>'],
      ['Serve', '<p>The file is streamed to the client. The next request for it does not leave your network.</p>']]) };

  P.docker = { src: 'docs/guides/docker.md', title: 'Docker & OCI', lead: 'Cache Docker Hub and other registries, and push your own images. Docker, buildx, containerd, Helm (OCI charts), cosign, crane and other OCI clients work.', body:
    fmtPage({ name: 'Docker', slug: 'docker', what: 'images', upstream: 'Docker Hub', upstreamUrl: 'https://hub.docker.com', names: { proxy: 'docker-hub' },
      client: '<p>OCI clients talk to <code>/v2/</code> at the root of a host, so the first part of the image name after the host is the Hangar repository.</p>' +
        code('shell', '$ echo "$HANGAR_TOKEN" | docker login ' + H + ' -u <user> --password-stdin\n$ docker pull ' + H + '/docker-group/library/nginx:1.27\n$ docker pull ' + H + '/docker-hub/busybox:1.36   # library/ is added for one-part names') +
        '<h3>A host of its own</h3><p>To drop the repository prefix from image names, give one Docker repository a <strong>connector</strong>: its own host name (<code>"connector": {"host": "cr.example.com"}</code>) or its own plain-HTTP port (<code>"http_port": 5001</code>). Then <code>docker pull cr.example.com/team/app:1.0</code> reads from that repository.</p>',
      publish: code('shell', '$ docker build -t ' + H + '/docker-hosted/team/app:1.0 .\n$ docker push ' + H + '/docker-hosted/team/app:1.0') + '<p>Pushing to the group stores the image in its <code>write_member</code>.</p>',
      notes: '<ul><li>OCI image indexes and Docker manifest lists are supported; multi-arch builds with <code>docker buildx build --platform linux/amd64,linux/arm64 --push</code> work. Docker schema 1 manifests are rejected.</li><li>Layers are stored once per blob store, whatever repository they belong to.</li><li>Set <code>HANGAR_DOCKER_TOKEN_KEY</code> (same value on every node), or Docker logins and unfinished uploads stop working after a restart.</li><li>cosign signatures and Helm OCI charts are stored next to images: <code>helm push chart-0.1.0.tgz oci://' + H + '/docker-hosted/charts</code>.</li><li>With <code>"write_policy": "allow_once"</code> a tag cannot be moved to another manifest.</li><li>The reverse proxy in front must not limit body size, or large layers fail with 413.</li></ul>' }) };

  P.npm = { src: 'docs/guides/npm.md', title: 'npm', lead: 'Works with npm, pnpm, yarn classic and yarn berry. Scoped packages, dist-tags, deprecate and unpublish are supported.', body:
    fmtPage({ name: 'npm', slug: 'npm', what: 'packages', upstream: 'registry.npmjs.org', upstreamUrl: 'https://www.npmjs.com',
      client: '<p>Add an <code>.npmrc</code> to the project, or to your home folder for every project. npm expands <code>${HANGAR_TOKEN}</code> from the environment, so the file can be committed:</p>' +
        code('ini', 'registry=' + R + 'npm-group/\n//' + H + SITE.repoPath + 'npm-group/:_authToken=${HANGAR_TOKEN}', '.npmrc') +
        tabs([['pnpm', code('shell', '$ pnpm install   # reads .npmrc')], ['yarn berry', code('yaml', 'npmRegistryServer: "' + R + 'npm-group/"\nnpmAlwaysAuth: true\nnpmAuthToken: "${HANGAR_TOKEN}"', '.yarnrc.yml')], ['yarn classic', code('ini', 'registry=' + R + 'npm-group/\n//' + H + SITE.repoPath + 'npm-group/:_authToken=${HANGAR_TOKEN}\nalways-auth=true', '.npmrc')]]),
      publish: '<p>Publish to the hosted repository, or to the group, which forwards to its <code>write_member</code>:</p>' + code('json', '{\n  "name": "@acme/ui-kit",\n  "publishConfig": { "registry": "' + R + 'npm-hosted/" }\n}', 'package.json') + code('shell', '$ npm publish'),
      notes: '<ul><li>Published versions never change, and an unpublished version number cannot be reused unless the repository allows it.</li><li><code>npm audit</code> gets real answers: proxies ask their remote, hosted repositories answer from Hangar\'s own findings, groups merge both. npm 10 and older hide audit errors; use npm 11 or pnpm where the result matters.</li><li>To route one scope only: <code>@acme:registry=' + R + 'npm-hosted/</code>.</li></ul>' }) };

  P.nuget = { src: 'docs/guides/nuget.md', title: 'NuGet', lead: 'NuGet V3 feeds: dotnet restore, add package, nuget push, nuget delete and package search.', body:
    fmtPage({ name: 'NuGet', slug: 'nuget', what: 'packages', upstream: 'nuget.org', upstreamUrl: 'https://www.nuget.org',
      client: code('xml', '<?xml version="1.0" encoding="utf-8"?>\n<configuration>\n  <packageSources>\n    <clear />\n    <add key="hangar" value="' + R + 'nuget-group/index.json" />\n  </packageSources>\n  <packageSourceCredentials>\n    <hangar>\n      <add key="Username" value="token" />\n      <add key="ClearTextPassword" value="%HANGAR_TOKEN%" />\n    </hangar>\n  </packageSourceCredentials>\n</configuration>', 'nuget.config') +
        '<p>The username is not checked when the password is a token. Or add the source from the command line:</p>' +
        code('shell', '$ dotnet nuget add source ' + R + 'nuget-group/index.json \\\n    --name hangar --username token --password "$HANGAR_TOKEN" --store-password-in-clear-text'),
      publish: code('shell', '$ dotnet pack -c Release -o out\n$ dotnet nuget push out/Ledger.Core.7.0.0.nupkg \\\n    -s ' + R + 'nuget-hosted/index.json -k "$HANGAR_TOKEN"'),
      notes: '<ul><li>Symbol packages (<code>.snupkg</code>) are accepted and stored next to the package.</li><li><code>dotnet nuget delete</code> unlists by default (<code>delete_mode: unlist</code>).</li><li>Pushing a version that already exists answers 409 unless the write policy is <code>allow</code>; use <code>--skip-duplicate</code>.</li><li>NuGet Audit works: hosted repositories serve Hangar\'s findings, proxies pass nuget.org\'s vulnerability data through.</li><li><code>&lt;clear /&gt;</code> makes Hangar the only source.</li></ul>' }) };

  P.maven = { src: 'docs/guides/maven.md', title: 'Maven & Gradle', lead: 'The Maven 2 repository layout for Maven and Gradle. Releases and snapshots live in separate hosted repositories.', body:
    fmtPage({ name: 'Maven', what: 'artifacts', rows: [
        ['<code>maven-central</code>', 'proxy', 'Caches <a href="https://repo1.maven.org/maven2/">Maven Central</a>. Artifacts are checked against the remote checksums.'],
        ['<code>maven-releases</code>, <code>maven-snapshots</code>', 'hosted', 'Release and SNAPSHOT artifacts your team deploys (<code>version_policy</code> <code>release</code> and <code>snapshot</code>).'],
        ['<code>maven-public</code>', 'group', 'One read-only URL for resolution that combines all three.']],
      client: tabs([
        ['Maven', code('xml', '<settings>\n  <servers>\n    <server>\n      <id>hangar</id>\n      <username>your-user</username>\n      <password>${env.HANGAR_TOKEN}</password>\n    </server>\n  </servers>\n  <mirrors>\n    <mirror>\n      <id>hangar</id>\n      <mirrorOf>*</mirrorOf>\n      <url>' + R + 'maven-public/</url>\n    </mirror>\n  </mirrors>\n</settings>', '~/.m2/settings.xml')],
        ['Gradle', code('kotlin', 'repositories {\n    maven {\n        name = "hangar"\n        url = uri("' + R + 'maven-public/")\n        credentials(PasswordCredentials::class)   // hangarUsername / hangarPassword\n    }\n}', 'build.gradle.kts')]]),
      publish: code('xml', '<distributionManagement>\n  <repository>\n    <id>hangar</id>\n    <url>' + R + 'maven-releases/</url>\n  </repository>\n  <snapshotRepository>\n    <id>hangar</id>\n    <url>' + R + 'maven-snapshots/</url>\n  </snapshotRepository>\n</distributionManagement>', 'pom.xml') + code('shell', '$ mvn -B deploy') + '<p>Gradle publishes with the <code>maven-publish</code> plugin to the same URLs.</p>',
      notes: '<ul><li>Maven groups are read-only: deploy to the hosted repositories, resolve through the group.</li><li><code>maven-metadata.xml</code> and checksum files are generated by Hangar.</li><li>Maven 3.8.1 and later block plain-HTTP repositories other than localhost; serve Hangar over HTTPS.</li></ul>' }) };

  P.pypi = { src: 'docs/guides/pypi.md', title: 'PyPI', lead: 'The Simple Repository API (PEP 503 and PEP 691) for pip, uv and Poetry. Upload with twine, uv publish or poetry publish.', body:
    fmtPage({ name: 'PyPI', slug: 'pypi', what: 'packages', upstream: 'pypi.org', upstreamUrl: 'https://pypi.org',
      client: '<p>Credentials are HTTP Basic: username <code>__token__</code> (any value works) and the token as the password.</p>' + tabs([
        ['pip', code('ini', '[global]\nindex-url = https://__token__:<token>@' + H + SITE.repoPath + 'pypi-group/simple/', '~/.config/pip/pip.conf')],
        ['uv', code('toml', '[[tool.uv.index]]\nname = "hangar"\nurl = "' + R + 'pypi-group/simple/"\ndefault = true', 'pyproject.toml') + code('shell', '$ export UV_INDEX_HANGAR_USERNAME=__token__\n$ export UV_INDEX_HANGAR_PASSWORD="$HANGAR_TOKEN"')],
        ['Poetry', code('shell', '$ poetry source add --priority=primary hangar ' + R + 'pypi-group/simple/\n$ poetry config http-basic.hangar __token__ "$HANGAR_TOKEN"')]]),
      publish: code('shell', '$ python -m build\n$ twine upload --repository-url ' + R + 'pypi-hosted/ \\\n    -u __token__ -p "$HANGAR_TOKEN" dist/*'),
      notes: '<ul><li>The upload URL is the repository root; the index URL ends in <code>/simple/</code>.</li><li>Yanked releases stay yanked; yank and un-yank through <code>' + R + 'pypi-hosted/yank/&lt;project&gt;/&lt;version&gt;</code>.</li><li>Name internal projects so they cannot collide with pypi.org projects, or add <a href="#routing">routing rules</a>.</li></ul>' }) };

  P.go = { src: 'docs/guides/go.md', title: 'Go modules', lead: 'A GOPROXY-compatible module proxy. Private modules are uploaded to hosted repositories as module zips.', body:
    fmtPage({ name: 'Go', slug: 'go', what: 'modules', upstream: 'proxy.golang.org', upstreamUrl: 'https://proxy.golang.org',
      client: code('shell', '$ go env -w GOPROXY=' + R + 'go-group\n$ go env -w GOPRIVATE=git.example.com/acme   # your private module prefix\n$ go env -w GONOPROXY=none                   # ...but still fetch it through Hangar\n$ go env -w GONOSUMDB=git.example.com/acme') +
        code('text', 'machine ' + H + '\nlogin token\npassword <token>', '~/.netrc') + '<p>The go command sends credentials only over HTTPS. For public modules only, <code>GOPROXY=' + R + 'go-proxy,direct</code> is enough.</p>',
      publish: '<p>A version is published by uploading its module zip:</p>' + code('shell', '$ curl -u token:"$HANGAR_TOKEN" --upload-file v1.2.3.zip \\\n    ' + R + 'go-hosted/git.example.com/acme/lib/@v/v1.2.3.zip') + '<p>Hangar checks the zip against the Go module zip rules and answers 409 if the version exists with other content.</p>',
      notes: '<ul><li><code>.info</code>, <code>.mod</code> and <code>.zip</code> of a version are fetched once and kept; version lists are refreshed after the metadata TTL (default 30 minutes).</li><li>A remote 404 or 410 becomes 404, so <code>,direct</code> falls back as the go command expects.</li><li>The checksum database is proxied at <code>' + R + 'go-proxy/sumdb/sum.golang.org/</code>.</li></ul>' }) };

  P.helm = { src: 'docs/guides/helm.md', title: 'Helm charts', lead: 'Two ways to store charts: classic chart repositories (index.yaml, helm repo add) in Helm repositories, and OCI charts in Docker repositories.', body:
    '<h2>Chart repositories</h2><p>Create <code>helm-hosted</code>, <code>helm-proxy</code> (any chart repository URL) and <code>helm-group</code> under <strong>Repositories → New repository → Helm</strong>.</p>' +
    code('shell', '$ helm repo add hangar ' + R + 'helm-group \\\n    --username token --password "$HANGAR_TOKEN"\n$ helm repo update\n$ helm install web hangar/web --version 1.2.0') +
    '<h3>Upload</h3>' + code('shell', '$ helm package ./web\n$ curl -fu "token:$HANGAR_TOKEN" --upload-file web-1.2.0.tgz \\\n    ' + R + 'helm-hosted/web-1.2.0.tgz') +
    '<p>The <code>helm cm-push</code> plugin works too (ChartMuseum upload API), and provenance files (<code>.prov</code>) are stored with the chart.</p>' +
    '<h2>OCI charts</h2><p>Helm 3.8+ stores charts as OCI artifacts in any <a href="#docker">Docker repository</a>:</p>' +
    code('shell', '$ echo "$HANGAR_TOKEN" | helm registry login ' + H + ' -u <user> --password-stdin\n$ helm push web-1.2.0.tgz oci://' + H + '/docker-hosted/charts\n$ helm install web oci://' + H + '/docker-group/charts/web --version 1.2.0') +
    '<p>Against a plain-HTTP server add <code>--plain-http</code>.</p>' };

  P.cargo = { src: 'docs/guides/cargo.md', title: 'Cargo', lead: 'Cargo\'s sparse registry protocol and web API: build, add, publish, yank and search. Needs cargo 1.74 or newer.', body:
    fmtPage({ name: 'Cargo', slug: 'cargo', what: 'crates', upstream: 'crates.io', upstreamUrl: 'https://crates.io', names: { proxy: 'crates-io' },
      client: code('toml', '[registry]\nglobal-credential-providers = ["cargo:token"]\n\n[registries.cargo-hosted]\nindex = "sparse+' + R + 'cargo-hosted/index/"\n\n[registries.cargo-group]\nindex = "sparse+' + R + 'cargo-group/index/"\n\n[source.crates-io]\nreplace-with = "cargo-group"', '~/.cargo/config.toml') +
        code('shell', '$ echo "$HANGAR_TOKEN" | cargo login --registry cargo-group\n$ echo "$HANGAR_TOKEN" | cargo login --registry cargo-hosted'),
      publish: code('shell', '$ cargo publish --registry cargo-hosted'),
      notes: '<ul><li>Crate files from crates.io are verified against the index checksum and cached.</li><li>Index files are generated on every request with <code>ETag</code>, so cargo revalidates cheaply.</li><li>In CI, set <code>CARGO_REGISTRIES_CARGO_GROUP_TOKEN</code> instead of <code>cargo login</code>.</li></ul>' }) };

  P.apt = { src: 'docs/guides/apt.md', title: 'APT', lead: 'Debian and Ubuntu repositories in the Debian archive layout, with signed InRelease files. apt-get update, install and unattended upgrades work.', body:
    '<h2>Repositories</h2>' + table(['Type', 'What it does'], [
      ['hosted', 'Stores your <code>.deb</code> packages and regenerates <code>Packages</code> and <code>Release</code> after every change, signed with the repository\'s key.'],
      ['proxy', 'Caches a Debian or Ubuntu mirror (default remote <code>http://deb.debian.org/debian</code>). Release files pass through unchanged.'],
      ['group', 'Combines <strong>hosted</strong> repositories only, re-signed with the group\'s key. Point clients at a proxy directly.']]) +
    '<p>Signing keys are stored encrypted with <code>HANGAR_SECRET_KEY</code>; without it, <code>InRelease</code> and the public key answer 501.</p>' +
    '<h2>Point apt at Hangar</h2>' + code('shell', '$ curl -fsSL -u <user>:<token> ' + R + 'apt-hosted/public.gpg \\\n    | sudo tee /etc/apt/keyrings/apt-hosted.gpg > /dev/null\n$ sudo tee /etc/apt/sources.list.d/apt-hosted.sources <<\'EOF\'\nTypes: deb\nURIs: ' + R + 'apt-hosted/\nSuites: stable\nComponents: main\nSigned-By: /etc/apt/keyrings/apt-hosted.gpg\nEOF\n$ echo \'machine ' + H + ' login <user> password <token>\' \\\n    | sudo tee /etc/apt/auth.conf.d/hangar.conf > /dev/null\n$ sudo apt-get update') +
    '<h2>Publish</h2>' + code('shell', '$ curl -u <user>:<token> --upload-file ./hello_2.10-3_amd64.deb \\\n    "' + R + 'apt-hosted/upload?distribution=bookworm&component=main"') +
    '<p>Distribution and component default to <code>stable</code> and <code>main</code>. Scanning needs the repository\'s distribution release (for example <code>Debian:12</code>); see <a href="#vulnerabilities">Vulnerability scanning</a>.</p>' };

  P.rpm = { src: 'docs/guides/rpm.md', title: 'RPM', lead: 'YUM/DNF repositories in the createrepo layout for RHEL, Rocky Linux, AlmaLinux and Fedora, with a signed repomd.xml.', body:
    '<h2>Repositories</h2>' + table(['Type', 'What it does'], [
      ['hosted', 'Stores your <code>.rpm</code> files and regenerates <code>repodata/</code> after every upload or delete.'],
      ['proxy', 'Caches one mirror, for example <code>https://dl.rockylinux.org/pub/rocky/9/BaseOS/x86_64/os/</code>. Mirror lists and metalinks are not followed.'],
      ['group', 'Combines <strong>hosted</strong> repositories only, signed with the group\'s key. Configure proxies as their own <code>.repo</code> entries.']]) +
    '<h2>Point dnf at Hangar</h2>' + code('ini', '[rpm-hosted]\nname=rpm-hosted\nbaseurl=' + R + 'rpm-hosted/\nenabled=1\nrepo_gpgcheck=1\ngpgkey=' + R + 'rpm-hosted/public.key\ngpgcheck=0\nusername=<user>\npassword=<token>', '/etc/yum.repos.d/rpm-hosted.repo') +
    code('shell', '$ sudo dnf makecache --repo rpm-hosted\n$ sudo dnf install hello') +
    '<h2>Publish</h2>' + code('shell', '$ curl -u <user>:<token> --upload-file ./hello-2.10-1.el9.x86_64.rpm \\\n    ' + R + 'rpm-hosted/Packages/h/hello-2.10-1.el9.x86_64.rpm') +
    '<p>Set <code>gpgcheck=1</code> when your packages are signed. Proxied packages keep their distribution signature.</p>' };

  P.raw = { src: 'docs/guides/raw.md', title: 'Raw files', lead: 'Store any file by path: build outputs, installers, tarballs, scripts. Any HTTP client works.', body:
    fmtPage({ name: 'Raw', slug: 'raw', what: 'files', upstream: 'any HTTP server', upstreamUrl: '#proxies', groupText: 'Serves a file from the first member that has it; listings merge all members. Read-only: upload to the hosted repository.',
      client: '<p>Use HTTP Basic (any username, the token as the password) or a Bearer token.</p>' + code('shell', '$ curl -u <user>:"$HANGAR_TOKEN" -fLO ' + R + 'raw-group/tools/1.0/app-1.0.tar.gz\n$ wget --user <user> --password "$HANGAR_TOKEN" ' + R + 'raw-group/tools/1.0/app-1.0.tar.gz'),
      publish: code('shell', '$ curl -u <user>:"$HANGAR_TOKEN" --upload-file app-1.0.tar.gz \\\n    ' + R + 'raw-hosted/tools/1.0/app-1.0.tar.gz'),
      notes: '<ul><li>Write policies: <code>allow</code> overwrites, <code>allow_once</code> accepts only identical re-uploads (409 otherwise), <code>deny</code> is read-only.</li><li>Directory listings are served as HTML, or JSON with <code>Accept: application/json</code>.</li><li>Raw files have no advisory source, so they are not scanned.</li></ul>' }) };

  P.proxies = { src: 'docs/guides/README.md', title: 'Proxies & caching', lead: 'A proxy repository fetches a file from its remote once, stores it, and serves every later request from Hangar.', body:
    '<h2>What happens on a request</h2><ol><li>Package files (tarballs, jars, layers, wheels) that are cached are served at once. Most of them never change and stay until a cleanup policy removes them.</li><li>Metadata (package documents, indexes, tag lists) is cached for the metadata TTL, then checked with the remote again. If the remote is unreachable, the cached copy is served.</li><li>A file that is not cached is downloaded, checked, stored by digest and served.</li><li>A remote 404 is remembered for the not-found TTL, so a typo does not hit the remote on every build.</li></ol>' +
    call('ok', 'Keeps working during outages.', 'When a remote is down or rate-limits you, everything already cached keeps working. A remote\'s 429 is passed on only for content Hangar does not have.') +
    '<h2>Settings</h2>' + table(['Setting (<code>config.proxy</code>)', 'In the UI', 'Meaning'], [
      ['<code>remote_url</code>', 'Remote URL', 'Where files come from (required).'],
      ['<code>metadata_ttl</code>', 'Metadata cached for', 'How long indexes and tag lists are trusted before checking again. Default per format, for example 30 minutes for npm, PyPI and Go.'],
      ['<code>content_ttl</code>', 'Content cached for', '<code>0s</code> keeps files forever.'],
      ['<code>negative_cache_ttl</code>', 'Not-found cached for', 'How long a remote 404 is remembered.'],
      ['<code>auth</code>', 'Authentication', 'Upstream credentials: basic, bearer or a custom header. Stored encrypted; needs <code>HANGAR_SECRET_KEY</code>.'],
      ['<code>offline</code>', '', '<code>true</code>: never contact the remote; serve only what is cached.'],
      ['<code>related_hosts</code>', '', 'Other hosts the remote links to (file CDNs) that may be fetched on its behalf, without its credentials.']]) +
    '<p>To make a proxy forget remembered 404s and rate limits: <code>POST /api/v1/repositories/&lt;name&gt;/cache:invalidate</code>. Cached content stays.</p>' +
    '<h2>Outbound proxy</h2><p>If your network blocks direct internet access, add an outbound HTTP proxy under <strong>Settings → Proxies</strong>, or set <code>http.proxy</code> in <code>hangar.yaml</code>. When remotes are reachable directly, there is nothing to fill in.</p>' + fig('proxies', 'Outbound proxy: nothing to fill in when Hangar can connect directly.') +
    '<p>Proxy repositories cannot reach loopback, private or cloud metadata addresses by default. To proxy an internal server, list it in <code>http.allow_private_networks</code>. A private CA goes in <code>http.ca_file</code>.</p>' +
    '<h2>Remote health</h2><p>Each remote is <strong>healthy</strong>, <strong>slow</strong>, <strong>blocked</strong> (401/403 or the address policy) or <strong>offline</strong> (the circuit breaker is open). The Overview <strong>Upstream</strong> card, the <strong>Upstream proxies</strong> row of System status and the repository list show it.</p>' +
    '<h2>Warming the cache</h2><p>A <strong>Pre-warm a proxy</strong> task (<strong>Scheduled tasks → New task</strong>) fetches a list of coordinates through a proxy or group, exactly as a client would. Paste coordinates or add them from lockfiles such as <code>package-lock.json</code>, <code>yarn.lock</code>, <code>packages.lock.json</code>, <code>poetry.lock</code>, <code>go.sum</code> or <code>Cargo.lock</code>. See <a href="#migrate">Move to Hangar</a>.</p>' };

  P.vulnerabilities = { src: 'docs/guides/vulnerabilities.md', title: 'Vulnerability scanning', lead: 'Hangar checks the package versions it stores against public advisories and shows what to upgrade first.', body:
    fig('vulnerabilities', 'The fix plan puts packages your builds actually use first.') +
    '<h2>What gets scanned</h2>' + table(['Format', 'Source', 'Matched by'], [
      ['npm, Maven, PyPI, NuGet, Go, Cargo', '<a href="https://osv.dev">OSV</a> (or a mirror of its API)', 'Package name and version'],
      ['APT, RPM', 'OSV', 'Source package and version, once the repository\'s distribution release is set or detected (for example <code>Debian:12</code>, <code>Rocky Linux:9</code>)'],
      ['Docker images', 'Trivy, when a Trivy server is configured', 'OS packages and application libraries inside the image'],
      ['Raw files', 'none', 'Not scanned']]) +
    '<p>Image scanning is optional: <code>install.sh --with-trivy</code>, or <code>imageScanning.enabled</code> in the Helm chart. Without it images show <em>Image scanning not configured</em>, never <em>Clean</em>.</p>' +
    '<h2>When</h2><ul><li><strong>On arrival</strong>, in repositories with <strong>Scan new packages for vulnerabilities</strong> on (<code>scan_on_arrival</code>).</li><li><strong>Every hour</strong>, the <code>vulnerability-rescan</code> task scans versions never scanned, failed scans, and versions whose last scan is older than <code>scanning.rescan_interval</code> (24 hours by default), so new advisories are picked up.</li><li><strong>On demand</strong>: <strong>Rescan</strong> on the Vulnerabilities screen, or the <a href="#api-vulnerabilities">API</a>.</li></ul>' +
    '<h2>Quarantine</h2><p>Hangar has one blocking rule, set per repository: <strong>Quarantine packages with critical issues</strong> (<code>quarantine_critical</code>, off by default). A version with an open critical finding is quarantined, and downloads answer <strong>403</strong>. The quarantine is lifted when the finding is fixed or waived, or when the setting is turned off. NuGet and Maven leave quarantined versions out of their indexes, so version ranges resolve to a version that can be downloaded.</p>' +
    call('', 'Everything else is information.', 'Findings of other severities are shown on the Vulnerabilities screen, the package page, in search (<code>security:findings</code>, <code>cve:CVE-2024-21907</code>) and to <code>npm audit</code> and NuGet Audit; they do not block downloads.') +
    '<h2>The fix plan</h2><p>The Vulnerabilities screen has one row per package: its vulnerable versions, the findings, the version to upgrade to, and how often builds pulled it in the last 7 days. A version is <strong>in use</strong> when it was downloaded within <code>scanning.in_use_window</code> (30 days). <strong>Copy upgrade</strong> copies the command for the ecosystem, and <strong>Export CSV</strong> downloads the table.</p>' +
    '<h2>Waivers</h2><p>When a finding does not apply to you, choose <strong>Waive</strong>. A waiver needs a reason and an end date at most a year ahead, needs the <code>system:security:write</code> privilege and is audited. Waived findings do not count as open and do not quarantine; when the waiver ends, the finding opens again.</p>' };

  P.routing = { src: 'docs/guides/routing-rules.md', title: 'Routing rules', lead: 'Keep internal package names on the repositories that own them, so a public package with the same name never reaches your builds.', body:
    '<p>Dependency confusion: someone publishes your internal name with a higher version on a public registry, and a group of your hosted repository plus a proxy of that registry serves the public copy. Routing rules prevent it.</p>' +
    table(['Repository', 'Rules', 'Effect'], [
      ['proxy', '<code>block</code>, <code>allow</code>', 'A blocked name is never requested from the remote and never served from the cache. With <code>allow</code> set, every other name is blocked. <code>block</code> wins.'],
      ['group', 'per member: <code>exclusive</code>, <code>block</code>', '<code>exclusive</code> reserves names for a member; <code>block</code> keeps names away from one member.']]) +
    '<h2>Recommended setup</h2>' + code('shell', '# 1. The public proxy never fetches internal names\n$ curl -X PUT ' + URL + '/api/v1/repositories/nuget-proxy/routing \\\n    -H "Authorization: Bearer $HANGAR_TOKEN" -H \'Content-Type: application/json\' \\\n    -d \'{"block": ["Acme.*"]}\'\n\n# 2. In the group, Acme.* comes only from the hosted member\n$ curl -X PUT ' + URL + '/api/v1/repositories/nuget-group/routing \\\n    -H "Authorization: Bearer $HANGAR_TOKEN" -H \'Content-Type: application/json\' \\\n    -d \'{"members": [{"member": "nuget-hosted", "exclusive": ["Acme.*"]}]}\'') +
    '<p>In the web UI: <strong>Repositories → (repository) → Settings → Routing rules</strong>. <strong>Block names your hosted repositories publish</strong> suggests patterns from the names they already hold.</p>' +
    '<h2>Patterns</h2><p><code>*</code> matches any run of characters, <code>?</code> one character. Each format matches its own identity: NuGet package ids (case-insensitive), npm <code>@scope/name</code>, Maven <code>groupId:artifactId</code>, normalized PyPI names, Go module paths, Cargo crate names, Docker image paths, Helm chart names, APT package names.</p>' };

  P.migrate = { src: 'docs/guides/migrate.md', title: 'Move to Hangar', lead: 'Import what you published into hosted repositories, pre-warm the proxies, switch the clients and verify.', body:
    steps([
      ['Import hosted packages', '<p><strong>Scheduled tasks → New task → Import packages</strong> (task type <code>repository_import</code>) copies packages from another registry into a hosted repository. It reads the source with the format\'s own protocol, so it works with any registry that speaks it, and writes through Hangar\'s normal checks, scanning and audit.</p>'],
      ['Pre-warm the proxies', '<p><strong>Pre-warm a proxy</strong> (<code>proxy_prewarm</code>) fetches coordinates through a proxy or group, from a pasted list, from lockfiles, or from everything a source registry holds.</p>'],
      ['Switch the clients', '<p>Point each client at the group of its format: <code>.npmrc</code>, <code>nuget.config</code>, Maven <code>settings.xml</code>, <code>pip.conf</code>, <code>GOPROXY</code>, <code>.cargo/config.toml</code>, <code>helm repo add</code>, Docker image names, APT and RPM sources.</p>'],
      ['Verify', '<p>Clean builds succeed against Hangar, the task runs ended without failures, and the Overview shows cache hits on the proxies.</p>']]) +
    code('shell', '$ curl -X POST ' + URL + '/api/v1/tasks \\\n    -H "Authorization: Bearer $HANGAR_TOKEN" -H \'Content-Type: application/json\' -d \'{\n      "name": "import-npm",\n      "type": "repository_import",\n      "config": {\n        "repository": "npm-hosted",\n        "source": { "url": "https://old.example.com/repository/npm-hosted/", "username": "mover", "password": "..." },\n        "packages": ["@acme/*"],\n        "latest": 20\n      }\n    }\'\n$ curl -X POST -H "Authorization: Bearer $HANGAR_TOKEN" ' + URL + '/api/v1/tasks/import-npm/run') +
    '<p>Imports exist for npm, NuGet, Maven, PyPI, Go, Cargo, Helm, Docker, raw, APT and RPM. A version already present with identical content is skipped; different content is reported as a conflict and left as it is. Runs resume after a restart, and running a task again copies only what is missing.</p>' };

  P.status = { src: 'docs/operations/monitoring.md', title: 'Health & status', lead: 'The System status screen answers one question first: is Hangar healthy? Details open when you click a row.', body:
    fig('status', 'System status: one row per check. Click a row for details.') +
    '<p><strong>Operations → System status</strong> shows every node with one row per subsystem: database, blob storage, event bus, task scheduler, search index, sign-in and upstream proxies. It refreshes every 15 seconds.</p>' +
    '<h2>Health endpoints</h2>' + table(['Path', 'Use', 'Returns'], [
      ['<code>/healthz</code>', 'Liveness probe', '200 <code>{"status":"ok"}</code> while the process runs'],
      ['<code>/readyz</code>', 'Readiness probe', '200 when the database, blob store and event bus (NATS) answer; 503 with the failing check otherwise, and while a node drains'],
      ['<code>/api/v1/system/status</code>', 'Status for scripts', 'State, version and dependency health; needs no sign-in'],
      ['<code>/metrics</code>', 'Prometheus', 'Requests, proxy cache hits, upstream state, blob storage, database pool, jobs']]) +
    code('yaml', 'livenessProbe:\n  httpGet: { path: /healthz, port: 8080 }\nreadinessProbe:\n  httpGet: { path: /readyz, port: 8080 }\n  periodSeconds: 10', 'Kubernetes') +
    '<p>The Helm chart sets these probes itself. Keep <code>/metrics</code> internal or protect it with <code>metrics.auth</code> (<code>bearer</code> or <code>basic</code>); the install script turns on bearer authentication. Grafana dashboards and alert rules are in <code>deploy/observability/</code>.</p>' +
    '<h2>Logs</h2><p>Logs are JSON lines on standard error, with secrets scrubbed. <strong>Operations → System logs</strong> shows each node\'s recent records, filters them and downloads them as JSON lines; attach that to bug reports.</p>' };

  P.ci = { src: 'docs/guides/ci.md', title: 'CI/CD pipelines', lead: 'Give each pipeline a service account token, scoped to what it needs.', body:
    '<h2>Create a service account and a token</h2><p>In <strong>Access & audit → Service accounts</strong>, or with the API:</p>' +
    code('shell', '$ curl -X POST ' + URL + '/api/v1/users \\\n    -H "Authorization: Bearer $ADMIN_TOKEN" -H \'Content-Type: application/json\' \\\n    -d \'{"username": "ci-publisher", "kind": "service_account", "roles": ["CI deployer"]}\'\n\n$ curl -X POST ' + URL + '/api/v1/tokens \\\n    -H "Authorization: Bearer $ADMIN_TOKEN" -H \'Content-Type: application/json\' \\\n    -d \'{"name": "gitlab-ci", "owner": "ci-publisher",\n         "scopes": ["repo:docker-hosted:read", "repo:docker-hosted:write"],\n         "expires_at": "2027-02-01T00:00:00Z"}\'') +
    '<p>Store the returned <code>secret</code> as a masked CI secret named <code>HANGAR_TOKEN</code>.</p>' +
    tabs([
      ['GitHub Actions', code('yaml', 'jobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n      - uses: docker/login-action@v3\n        with:\n          registry: ' + H + '\n          username: ci-publisher\n          password: ${{ secrets.HANGAR_TOKEN }}\n      - run: docker build -t ' + H + '/docker-hosted/app:${{ github.sha }} .\n      - run: docker push ' + H + '/docker-hosted/app:${{ github.sha }}', '.github/workflows/build.yml')],
      ['GitLab CI', code('yaml', 'build:\n  image: docker:27\n  services: [docker:27-dind]\n  script:\n    - echo "$HANGAR_TOKEN" | docker login ' + H + ' -u ci-publisher --password-stdin\n    - docker build -t ' + H + '/docker-hosted/app:$CI_COMMIT_SHORT_SHA .\n    - docker push ' + H + '/docker-hosted/app:$CI_COMMIT_SHORT_SHA', '.gitlab-ci.yml')],
      ['Jenkins', code('groovy', 'pipeline {\n  agent any\n  stages {\n    stage(\'Build\') {\n      steps {\n        withCredentials([usernamePassword(credentialsId: \'hangar-ci\',\n            usernameVariable: \'HANGAR_USER\', passwordVariable: \'HANGAR_TOKEN\')]) {\n          sh \'echo "$HANGAR_TOKEN" | docker login ' + H + ' -u "$HANGAR_USER" --password-stdin\'\n          sh \'docker build -t ' + H + '/docker-hosted/app:$BUILD_NUMBER .\'\n          sh \'docker push ' + H + '/docker-hosted/app:$BUILD_NUMBER\'\n        }\n      }\n    }\n  }\n}', 'Jenkinsfile')]]) +
    '<h2>Token tips</h2><ul><li>One service account per pipeline family and one token per CI system, so a leak can be revoked on its own.</li><li>Scope tokens to the repositories the job touches: <code>read</code> to resolve, <code>write</code> to publish. Patterns work: <code>repo:npm-*:write</code>.</li><li>Every token expires (at most 366 days). Tokens that expire within 14 days are listed on My account and under the notification bell.</li><li>Remove revoked tokens from CI quickly: about 30 failed authentications a minute from one address make every request from it answer 429 for a while, valid tokens included.</li></ul>' };

  P.configuration = { src: 'hangar.example.yaml', title: 'Configuration', lead: 'Hangar reads hangar.yaml, then HANGAR_* environment variables, which win over the file. Unknown keys stop the start, so typos fail loudly.', body:
    '<h2>Example file</h2>' + code('yaml', 'server:\n  addr: ":8080"\n  base_url: ' + URL + '          # public address; every generated link uses it\n  trusted_proxies: [10.0.0.5/32]          # your reverse proxy\n\ndatabase:\n  migrate_on_start: true\n  max_conns: 20\n\nstorage:\n  kind: fs                                # fs or s3\n  fs:\n    path: /var/lib/hangar/blobs\n\nnats:\n  embedded: true\n  data_dir: /var/lib/hangar/nats\n\nscanning:\n  enabled: true\n  rescan_interval: 24h', 'hangar.yaml') +
    code('shell', '# Secrets stay out of the file\nHANGAR_DATABASE_URL=postgres://hangar:PASSWORD@db:5432/hangar\nHANGAR_SECRET_KEY=<openssl rand -base64 32>\nHANGAR_DOCKER_TOKEN_KEY=<openssl rand -base64 48>', 'environment') +
    '<h2>Environment variables and secret files</h2><p>Every key maps to a variable: <code>HANGAR_</code> plus its path in upper case, joined with underscores. So <code>database.url</code> is <code>HANGAR_DATABASE_URL</code> and <code>storage.fs.path</code> is <code>HANGAR_STORAGE_FS_PATH</code>. Lists take comma-separated values.</p>' +
    '<p>Secrets can also be files in <code>secrets_dir</code>, one per setting, named after its path (<code>security.secret_key</code>, <code>auth.oidc.client_secret</code>). The install script uses <code>&lt;data&gt;/secrets</code>, and it works with Kubernetes secret volumes.</p>' +
    '<h2>Settings you will want early</h2>' + table(['Key', 'Default', 'Description'], [
      ['<code>server.base_url</code>', '<code>http://localhost:8080</code>', 'The URL clients use. npm tarball URLs, PyPI links and the Docker token realm are built from it.'],
      ['<code>server.addr</code>', '<code>:8080</code>', 'Address and port to listen on (plain HTTP).'],
      ['<code>server.trusted_proxies</code>', 'empty', 'Reverse proxies whose <code>X-Forwarded-*</code> headers are believed. Without it all clients share the proxy\'s rate limit.'],
      ['<code>database.url</code>', 'required', 'PostgreSQL 16+ connection string. Keep it in the environment.'],
      ['<code>database.migrate_on_start</code>', '<code>false</code>', 'Apply migrations on start; otherwise run <code>hangar migrate up</code>.'],
      ['<code>database.max_conns</code>', '<code>20</code>', 'Connection pool size per node.'],
      ['<code>storage.kind</code>', '<code>fs</code>', '<code>fs</code> or <code>s3</code>. See <a href="#storage">Storage</a>.'],
      ['<code>nats.embedded</code>', '<code>true</code>', 'Run NATS JetStream inside the process (one node). See <a href="#cluster">Clustering</a>.'],
      ['<code>security.secret_key</code>', 'none', '<code>HANGAR_SECRET_KEY</code>: encrypts upstream credentials and signing keys stored in the database. Same value on every node.'],
      ['<code>docker.token_key</code>', 'random per process', '<code>HANGAR_DOCKER_TOKEN_KEY</code>: signs registry tokens, so Docker logins survive restarts.'],
      ['<code>auth.oidc.*</code>, <code>auth.ldap.*</code>', 'off', 'Single sign-on; can also be set in the UI. See <a href="#authentication">Sign-in & tokens</a>.'],
      ['<code>scanning.enabled</code>', '<code>true</code>', 'OSV scanning of packages.'],
      ['<code>scanner.trivy_url</code>', 'empty', 'Trivy server for image scanning; empty means images are not scanned.'],
      ['<code>metrics.auth</code>', '<code>none</code>', '<code>none</code>, <code>bearer</code> or <code>basic</code> for <code>/metrics</code>.']]) +
    call('', 'The full reference.', guide('hangar.example.yaml') + ' documents every key with its default and variable. Settings are read at start, so change them on the host and restart. Sign-in providers and outbound proxies can also be edited under <strong>Settings</strong> in the UI.') };

  P.authentication = { src: 'docs/guides/access.md', title: 'Sign-in & tokens', lead: 'People sign in with local accounts, an OIDC provider or LDAP / Active Directory. Machines use access tokens.', body:
    fig('sign-in', 'Settings → Authentication: pick your provider and fill in three fields.') +
    '<h2>Single sign-on</h2><p>Open <strong>Settings → Authentication</strong>. Presets cover Active Directory, OpenLDAP and FreeIPA (LDAP), and Keycloak, Microsoft Entra ID, Google Workspace and Okta (OIDC); <strong>Other</strong> takes any OIDC provider or LDAP directory. <strong>Test</strong> checks the values before you save, and saved settings apply on every node.</p>' +
    steps([
      ['Register Hangar at your provider', '<p>Redirect URI: <code>' + URL + '/api/v1/auth/oidc/callback</code>.</p>'],
      ['Fill in the issuer, client ID and secret', '<p>In the UI, or in <code>hangar.yaml</code> (a provider set in the file is shown read-only in the UI):</p>' + code('yaml', 'auth:\n  oidc:\n    enabled: true\n    issuer_url: https://sso.example.com/realms/acme\n    client_id: hangar\n    # client_secret: HANGAR_AUTH_OIDC_CLIENT_SECRET\n    group_roles: { platform-team: [Publisher] }\n    default_roles: [Reader]')],
      ['Map groups to roles', '<p>Map provider groups to Hangar roles in the <strong>Groups and roles</strong> table, or under <strong>Access & audit → Roles → Directory groups</strong>. Users are created at their first sign-in.</p>']]) +
    '<p>LDAP users can also authenticate package clients with HTTP Basic. Local accounts keep working next to both.</p>' +
    '<h2>Roles</h2>' + table(['Role', 'Can'], [
      ['Reader', 'Read every repository.'], ['Publisher', 'Also publish to every repository.'], ['CI deployer', 'Read, publish and delete.'], ['Admin', 'Everything, including settings, users and repositories.']]) +
    '<p>Custom roles combine privileges such as <code>repo:npm-*:write</code> or <code>system:security:read</code>.</p>' +
    '<h2>Access tokens</h2><p>Use tokens (<code>hgr_…</code>) for CI, scripts and package clients. Create them under <strong>My account → Personal access tokens</strong>, or for a service account under <strong>Access & audit</strong>. Every token has scopes, an expiry of at most 366 days, and optionally allowed networks and a read-only flag. A token never does more than its owner. The secret is shown once.</p>' +
    '<p>Clients send the token with HTTP Basic (any username, the token as the password) or as a Bearer token:</p>' +
    code('shell', '$ curl -H "Authorization: Bearer $HANGAR_TOKEN" ' + URL + '/api/v1/me') +
    '<p>Five wrong passwords within 15 minutes lock an address out of that account for 15 minutes (423). Every change and every failed sign-in is written to the audit log.</p>' +
    call('danger', 'Lost a token?', 'Revoke it under <strong>My account → Personal access tokens</strong>, or <strong>Access & audit → Access tokens</strong> for someone else\'s. <strong>Rotate</strong> replaces a token and can keep the old one working for a grace period.') };

  P.storage = { src: 'docs/deploy/requirements.md', title: 'Storage & cleanup', lead: 'Package files live in blob stores. Metadata lives in PostgreSQL.', body:
    tabs([
      ['Filesystem', code('yaml', 'storage:\n  kind: fs\n  fs:\n    path: /var/lib/hangar/blobs')],
      ['S3 / MinIO', code('yaml', 'storage:\n  kind: s3\n  s3:\n    endpoint: ""                 # empty for AWS; https://minio.example.com for MinIO\n    region: eu-central-1\n    bucket: hangar-blobs\n    use_path_style: true         # usually needed for MinIO\n    access_key_id: AKIA...\n    # secret_access_key: HANGAR_STORAGE_S3_SECRET_ACCESS_KEY')]]) +
    '<p>This store is registered as <code>default</code> on the first start. More stores can be added with <code>POST /api/v1/blob-stores</code>. <strong>Storage → Blob stores</strong> shows usage, what deduplication saved and what can be reclaimed. Several nodes need S3 or one shared filesystem.</p>' +
    '<h2>How much space do I need?</h2><p>The sum of unique file bytes plus cached proxy content; identical files are stored once. The sizing guide starts a team of up to 50 users at 200 GiB of blob storage.</p>' +
    '<h2>Cleaning up</h2><p><strong>Storage → Cleanup policies</strong> delete components of chosen repositories by age, by the last download, by pre-release status or beyond a keep-newest count, for example <em>SNAPSHOTs older than 30 days, keeping the newest 5</em>. A new or changed rule must be previewed before it is saved. Policies run on a cron schedule (UTC) or on demand.</p>' +
    '<p>Deleted content is freed by blob garbage collection, which keeps unreferenced blobs for <code>storage.gc.grace_period</code> (48 hours) first.</p>' };

  P.cluster = { src: 'docs/deploy/high-availability.md', title: 'Clustering', lead: 'Run several Hangar nodes for availability. They share PostgreSQL, the blob store and a NATS JetStream cluster.', body:
    '<h2>When you need it</h2><p>Nodes are stateless: any node serves any request, without sticky sessions. Add nodes to survive a machine failure and to upgrade without downtime.</p>' +
    '<h2>Requirements</h2><ul><li>One PostgreSQL database for all nodes, made highly available by your platform.</li><li>S3 storage, or one filesystem mounted at the same path on every node.</li><li>A 3-node NATS JetStream cluster (the Helm chart can deploy one).</li><li>The same <code>hangar.yaml</code>, <code>HANGAR_SECRET_KEY</code> and <code>HANGAR_DOCKER_TOKEN_KEY</code> on every node.</li><li>A load balancer that checks <code>/readyz</code>.</li></ul>' +
    code('yaml', 'nats:\n  embedded: false\n  url: nats://nats-1:4222,nats://nats-2:4222,nats://nats-3:4222\n  replicas: 3', 'hangar.yaml on every node') +
    '<p>One node is the scheduler leader (a PostgreSQL advisory lock) and enqueues periodic jobs; any node runs them. If the leader stops, another takes over within seconds. <strong>Operations → System status</strong> lists every node with its role and checks.</p>' +
    '<p>On SIGTERM a node fails <code>/readyz</code> at once; <code>server.drain_delay</code> keeps it serving while the load balancer notices.</p>' };

  P.backups = { src: 'docs/deploy/backup-restore.md', title: 'Backups & restore', lead: 'Back up three things: the database, the blob store and the keys.', body:
    table(['What', 'Where'], [
      ['Metadata', 'PostgreSQL (<code>pg_dump</code>, custom format)'],
      ['Package files', 'The blob store: <code>storage.fs.path</code> or the S3 bucket'],
      ['Keys', '<code>HANGAR_SECRET_KEY</code> and <code>HANGAR_DOCKER_TOKEN_KEY</code>; with the install script, <code>&lt;data&gt;/secrets/</code>']]) +
    '<h2>Back up</h2><p><code>deploy/scripts/backup.sh</code> and <code>restore.sh</code> in the repository implement this. For an install-script installation:</p>' +
    code('shell', '$ HANGAR_COMPOSE_FILE=/srv/hangar/compose.yaml BLOB_SOURCE=fs:/srv/hangar/blobs \\\n  BACKUP_DIR=/srv/backups/hangar BACKUP_KEEP=14 \\\n  deploy/scripts/backup.sh') +
    call('', 'Order matters.', 'The script dumps the database first, then copies blobs. Garbage collection keeps unreferenced blobs for 48 hours, so a copy that finishes within that time has every blob the dump needs. Hangar keeps running.') +
    '<p>For S3, bucket versioning or replication is often easier than copying: then back up only the database (<code>BLOB_SOURCE=none</code>). The install script also dumps the database to <code>&lt;data&gt;/backups/</code> before every upgrade.</p>' +
    '<h2>Restore</h2>' + steps([
      ['Stop Hangar', code('shell', '$ cd /srv/hangar && sudo docker compose stop hangar')],
      ['Restore the database and blobs', code('shell', '$ HANGAR_COMPOSE_FILE=/srv/hangar/compose.yaml BLOB_SOURCE=fs:/srv/hangar/blobs \\\n  deploy/scripts/restore.sh /srv/backups/hangar/hangar-20261104T023000Z')],
      ['Use the same keys', '<p>Set the same <code>HANGAR_SECRET_KEY</code> and <code>HANGAR_DOCKER_TOKEN_KEY</code> as before.</p>'],
      ['Start and verify', code('shell', '$ sudo docker compose start hangar\n$ curl -f ' + URL + '/readyz')]]) };

  P.upgrading = { src: 'docs/deploy/upgrade.md', title: 'Upgrading', lead: 'Read the changelog, back up, pin the new version. Database migrations run on start or in a migration job.', body:
    steps([
      ['Read the release notes', '<p>On <a href="' + GH + '/releases">GitHub releases</a> and in ' + guide('docs/changelog.md', 'the changelog') + ', in particular new settings and migrations.</p>'],
      ['Back up', '<p>See <a href="#backups">Backups</a>. The install script also dumps the database before it upgrades.</p>'],
      ['Upgrade', tabs([
        ['Script', code('shell', '$ curl -fsSL ' + INSTALL + ' \\\n    | sudo bash -s -- --data-dir /srv/hangar --version vX.Y.Z') + '<p>It keeps data, secrets and settings, pulls the new images and restarts; migrations run on start.</p>'],
        ['Helm', code('shell', '$ helm upgrade hangar deploy/helm/hangar -n hangar -f values-prod.yaml \\\n    --set image.tag=<new version> --wait') + '<p>A hook job runs <code>hangar migrate up</code> before any pod is replaced.</p>'],
        ['Binary', code('shell', '$ sudo install -m 0755 hangar-linux-amd64 /usr/local/bin/hangar\n$ sudo systemctl restart hangar')]])],
      ['Check', '<p><code>/readyz</code> returns 200 and <strong>System status</strong> (or <code>/api/v1/system/status</code>) shows the new version.</p>']]) +
    '<p>With several nodes, roll out node by node only when the release\'s changelog says its migrations work with the previous version; otherwise stop all nodes, migrate, and start the new version.</p>' };

  function fig(img, caption) { return '<figure class="fig"><button type="button" class="zoom" aria-label="Enlarge screenshot"><img src="assets/img/' + img + '.jpg" alt="' + caption + '" loading="lazy" width="1600" height="1000"></button><figcaption>' + caption + '</figcaption></figure>'; }
  function api(o) {
    var h = '<div class="endpoint"><span class="verb ' + o.v.toLowerCase().slice(0, 4).replace('dele', 'del') + '">' + o.v + '</span>' + o.p + '</div><p>' + o.d + '</p>';
    if (o.params) h += table(['Parameter', 'In', 'Description'], o.params.map(function (r) { return ['<code>' + r[0] + '</code>', r[1], r[2]]; }));
    var curl = 'curl' + (o.v !== 'GET' ? ' -X ' + o.v : '') + ' "' + (o.ex.indexOf('http') === 0 ? o.ex : '$HANGAR' + o.ex) + '"' + (o.anon ? '' : ' \\\n  -H "Authorization: Bearer $HANGAR_TOKEN"') + (o.body ? ' \\\n  -H "Content-Type: application/json" \\\n  -d \'' + o.body.replace(/\n\s*/g, ' ') + '\'' : '');
    h += tabs([['Request', code('shell', '$ ' + curl)], ['Response', code('json', o.res || '204 No Content', o.status || '200 OK')]]);
    return '<div class="api-op">' + h + '</div>';
  }

  P.api = { src: 'docs/guides/api.md', title: 'API overview', lead: 'Everything the web UI does goes through a JSON REST API under /api/v1.', body:
    '<h2>Base URL</h2>' + code('shell', '$ export HANGAR=' + URL + '/api/v1') +
    '<p>Every server publishes its own reference with its real URLs: <code>/api-docs</code> in the web UI (also under Settings → API reference) and the OpenAPI 3.1 document at <code>/api/v1/openapi.yaml</code>. The same document is ' + guide('api/openapi.yaml', 'in the repository') + '.</p>' +
    '<h2>Authentication</h2><p>Send an access token as a Bearer token, or with HTTP Basic (any username, the token as the password). Create one under <strong>My account → Personal access tokens</strong> or with <a href="#api-tokens">the tokens API</a>. Every route needs sign-in except the status, version and sign-in routes and the OpenAPI documents.</p>' +
    code('shell', '$ curl "$HANGAR/me" -H "Authorization: Bearer $HANGAR_TOKEN"') +
    code('json', '{\n  "user": { "username": "ci-publisher", "kind": "service_account" },\n  "privileges": ["repo:npm-hosted:read", "repo:npm-hosted:write"],\n  "auth_method": "token",\n  "token": { "name": "release pipeline", "prefix": "hgr_1Hzo", "expires_at": "2027-02-01T00:00:00Z", "read_only": false }\n}', '200 OK (shortened)') +
    '<h2>Pagination</h2><p>List endpoints return <code>items</code> and a <code>next_cursor</code>. Pass it back unchanged as <code>?cursor=</code> to get the next page; it is <code>null</code> on the last page. <code>limit</code> is 50 by default, from 1 to 1000.</p>' +
    code('json', '{\n  "items": [ … ],\n  "next_cursor": "eyJvZmZzZXQiOjUwfQ"\n}') +
    '<h2>Errors</h2><p>Errors use normal HTTP status codes and the same body: <code>code</code>, <code>message</code>, and sometimes <code>retry_after</code> or <code>details</code>.</p>' +
    table(['Status', '<code>code</code>', 'When'], [
      ['400', '<code>invalid_argument</code>', 'Missing or malformed field.'],
      ['401', '<code>unauthenticated</code>', 'Missing, wrong, expired or revoked credentials.'],
      ['403', '<code>forbidden</code>, <code>read_only_token</code>, <code>quarantined</code>', 'Not allowed; a write with a read-only token; the component is quarantined.'],
      ['404', '<code>not_found</code>', 'Does not exist, or you may not read it.'],
      ['409', '<code>already_exists</code>, <code>write_denied</code>', 'Name taken; the write policy refused the upload.'],
      ['423', '<code>locked</code>', 'This address is locked out of the account after failed passwords.'],
      ['429', '<code>rate_limited</code>', 'Too many requests. See <code>Retry-After</code>.'],
      ['501', '<code>not_implemented</code>', 'Described in the OpenAPI document but not available yet.'],
      ['502', '<code>upstream_unavailable</code>', 'A proxy remote failed.'],
      ['503', '<code>offline</code>', 'The repository is offline.']]) +
    code('json', '{\n  "code": "invalid_argument",\n  "message": "repository name \\"x\\" must match ^[a-z0-9][a-z0-9-]{1,62}$: invalid argument"\n}', '400 Bad Request') +
    '<h2>Rate limits</h2><p>Per client address: 1200 API requests a minute (burst 300), 30 failed authentications a minute, and 10 sign-in attempts a minute. Behind a reverse proxy, set <code>server.trusted_proxies</code> so clients are told apart.</p>' +
    '<h2>Endpoints</h2>' + cards([
      ['api-repositories', 'Repositories', 'Create, list, change and delete repositories.'],
      ['api-packages', 'Components & search', 'Search, list versions and delete them.'],
      ['api-vulnerabilities', 'Vulnerabilities', 'Findings, waivers, rescans and the fix plan.'],
      ['api-tokens', 'Tokens & users', 'Access tokens, service accounts and users.'],
      ['api-system', 'System', 'Health, status and version.']]) };

  P['api-repositories'] = { src: 'docs/guides/api.md', title: 'Repositories API', lead: 'Manage hosted, proxy and group repositories. Changes need system:repos:admin or repo:<name>:admin.', body:
    '<h2>List repositories</h2>' + api({ v: 'GET', p: '/repositories', ex: '/repositories?format=npm', d: 'Repositories you can see, with counts per type.',
      params: [['format', 'query', 'apt, cargo, docker, go, helm, maven, npm, nuget, pypi, raw or rpm'], ['type', 'query', 'hosted, proxy or group'], ['q', 'query', 'Part of the name'], ['limit', 'query', 'Page size, default 50']],
      res: '{\n  "items": [\n    {\n      "name": "npm-proxy",\n      "format": "npm",\n      "type": "proxy",\n      "blob_store": "default",\n      "online": true,\n      "config": { "proxy": { "remote_url": "https://registry.npmjs.org", "metadata_ttl": "30m" } },\n      "url": "' + R + 'npm-proxy/",\n      "status": "online",\n      "size_bytes": 1834201122,\n      "component_count": 362\n    }\n  ],\n  "next_cursor": null,\n  "counts": { "all": 3, "hosted": 1, "proxy": 1, "group": 1 }\n}' }) +
    '<h2>Create a repository</h2>' + api({ v: 'POST', p: '/repositories', ex: '/repositories', d: 'Creates a repository. Proxies need <code>config.proxy.remote_url</code>, groups <code>config.group.members</code>. The same document is accepted as YAML (<code>Content-Type: application/yaml</code>).',
      params: [['name', 'body', 'Lower-case letters, digits and dashes; cannot change'], ['format', 'body', 'Package format; cannot change'], ['type', 'body', 'hosted, proxy or group; cannot change'], ['blob_store', 'body', 'Required for hosted and proxy; the first store is <code>default</code>'], ['config', 'body', '<code>hosted</code>, <code>proxy</code>, <code>group</code>, <code>anonymous_read</code>, <code>format</code>, <code>routing</code>']],
      body: '{\n  "name": "npm-proxy",\n  "format": "npm",\n  "type": "proxy",\n  "blob_store": "default",\n  "config": { "proxy": { "remote_url": "https://registry.npmjs.org" } }\n}',
      status: '201 Created', res: '{\n  "name": "npm-proxy",\n  "format": "npm",\n  "type": "proxy",\n  "url": "' + R + 'npm-proxy/",\n  "status": "online"\n}' }) +
    '<h2>Get one repository</h2>' + api({ v: 'GET', p: '/repositories/{name}', ex: '/repositories/npm-proxy', d: 'Settings and usage of one repository. Send <code>Accept: application/yaml</code> for YAML. Upstream secrets are never returned.',
      res: '{\n  "name": "npm-proxy",\n  "format": "npm",\n  "type": "proxy",\n  "online": true,\n  "config": { "proxy": { "remote_url": "https://registry.npmjs.org", "metadata_ttl": "30m" }, "anonymous_read": false },\n  "status": "online"\n}' }) +
    '<h2>Change settings</h2>' + api({ v: 'PUT', p: '/repositories/{name}', ex: '/repositories/npm-proxy', d: 'Send the whole document; name, format and type cannot change.', body: '{\n  "blob_store": "default",\n  "online": true,\n  "config": { "proxy": { "remote_url": "https://registry.npmjs.org", "offline": true } }\n}', res: '{\n  "name": "npm-proxy",\n  "config": { "proxy": { "remote_url": "https://registry.npmjs.org", "offline": true } }\n}' }) +
    '<h2>Delete a repository</h2>' + api({ v: 'DELETE', p: '/repositories/{name}', ex: '/repositories/npm-old', d: 'Deletes the repository. Its files are freed by blob garbage collection.', status: '204 No Content' }) };

  P['api-packages'] = { src: 'docs/guides/api.md', title: 'Components & search API', lead: 'A component is one package version; its files are assets. Search, inspect and delete them.', body:
    '<h2>Search</h2>' + api({ v: 'GET', p: '/search', ex: '/search?q=serilog+format:nuget', d: 'Searches every repository you can read. Free text matches name prefixes.',
      params: [['q', 'query', 'Text plus qualifiers: <code>format:</code>, <code>repo:</code>, <code>sha256:</code>, <code>license:</code>, <code>security:</code>, <code>cve:</code>, <code>unused:90d</code>'], ['sort', 'query', '<code>relevance</code>, <code>-downloads</code> or <code>-published_at</code>'], ['limit', 'query', 'Page size, default 50']],
      res: '{\n  "items": [\n    {\n      "component_id": "…",\n      "format": "nuget",\n      "coordinates": "Serilog 2.10.0",\n      "name": "Serilog",\n      "version": "2.10.0",\n      "repository": "nuget-proxy",\n      "versions": 1,\n      "downloads": 41,\n      "security": "clean"\n    }\n  ],\n  "next_cursor": null,\n  "total": 1\n}' }) +
    '<h2>List components</h2>' + api({ v: 'GET', p: '/components', ex: '/components?repository=nuget-proxy&name=Serilog', d: 'Package versions, newest first by default.',
      params: [['repository', 'query', 'Exact repository name'], ['format', 'query', 'One format'], ['name', 'query', 'Exact package name'], ['sort', 'query', '<code>-published_at</code> (default), <code>published_at</code>, <code>name</code>, <code>-downloads</code>']],
      res: '{\n  "items": [\n    {\n      "id": "…",\n      "format": "nuget",\n      "name": "Serilog",\n      "version": "2.10.0",\n      "repository": "nuget-proxy",\n      "published_at": "2026-11-04T12:31:08Z",\n      "download_count": 41,\n      "scan": { "state": "clean", "critical": 0, "high": 0, "medium": 0, "low": 0 }\n    }\n  ],\n  "next_cursor": null\n}' }) +
    '<p>More: <code>GET /components/{id}</code> (details, checksums, install command), <code>/components/{id}/assets</code> (files with download URLs) and <code>/components/{id}/versions</code>.</p>' +
    '<h2>Upload</h2><p>Packages are published with their own client (<code>npm publish</code>, <code>dotnet nuget push</code>, <code>docker push</code>, <code>twine upload</code>). Raw files are a plain HTTP PUT:</p>' +
    code('shell', '$ curl -u any:"$HANGAR_TOKEN" --upload-file app-1.0.tar.gz \\\n    ' + R + 'raw-hosted/tools/1.0/app-1.0.tar.gz') +
    '<h2>Delete a component</h2>' + api({ v: 'DELETE', p: '/components/{id}', ex: '/components/<id>', d: 'Removes one version and its files (needs <code>repo:&lt;name&gt;:delete</code>). Builds that pinned it will fail, so prefer cleanup policies.', status: '204 No Content' }) };

  P['api-vulnerabilities'] = { src: 'docs/guides/vulnerabilities.md', title: 'Vulnerabilities API', lead: 'Read findings, waive them, start a rescan and export the fix plan.', body:
    '<h2>List findings</h2>' + api({ v: 'GET', p: '/findings', ex: '/findings?severity=critical,high&fix_available=true', d: 'Findings you can see, with totals by severity.',
      params: [['severity', 'query', 'critical, high, medium, low (comma-separated)'], ['status', 'query', 'open, assigned, waived or fixed'], ['fix_available', 'query', '<code>true</code> for findings with a fixed version'], ['repository', 'query', 'One repository'], ['q', 'query', 'Package, advisory id or CVE']],
      res: '{\n  "items": [\n    {\n      "id": "…",\n      "advisory_id": "GHSA-g8r8-53c2-pm3f",\n      "aliases": ["CVE-2026-47304"],\n      "title": "…",\n      "severity": "high",\n      "cvss": 8.1,\n      "package": "System.Security.Cryptography.Xml",\n      "installed": "8.0.2",\n      "fixed_in": "8.0.4",\n      "status": "open",\n      "quarantined": false\n    }\n  ]\n}' }) +
    '<h2>Waive a finding</h2>' + api({ v: 'POST', p: '/findings/{id}/waiver', ex: '/findings/<id>/waiver', d: 'Accepts the risk until a date at most a year ahead. Needs <code>system:security:write</code>; audited. <code>DELETE</code> on the same path revokes it.',
      body: '{\n  "reason": "Code path not used; upgrade planned",\n  "until": "2026-12-31T23:59:59Z"\n}', res: '{\n  "id": "…",\n  "status": "waived"\n}' }) +
    '<h2>Rescan now</h2>' + api({ v: 'POST', p: '/vulnerabilities/rescan', ex: '/vulnerabilities/rescan', d: 'Queues a scan of every repository. <code>GET</code> on the same path shows its progress. Answers 501 when scanning is turned off.', status: '202 Accepted', res: '{\n  "run": { … },\n  "started": true\n}' }) +
    '<h2>Fix plan</h2>' + api({ v: 'GET', p: '/vulnerabilities/fix-plan', ex: '/vulnerabilities/fix-plan?tab=in_use&window=7d', d: 'The table of the Vulnerabilities screen. Add <code>format=csv</code> or <code>Accept: text/csv</code> for a spreadsheet.', res: '{ "items": [ … ] }' }) +
    '<p>More: <code>POST /repositories/{name}:scan</code>, <code>POST /components/{id}:scan</code>, <code>GET /scanning/status</code> and <code>GET /findings/trend</code>.</p>' };

  P['api-tokens'] = { src: 'docs/guides/api.md', title: 'Tokens & users API', lead: 'Create tokens for CI and scripts, and manage users and service accounts.', body:
    '<h2>Create a token</h2>' + api({ v: 'POST', p: '/tokens', ex: '/tokens', d: 'Creates a token for the caller, or for a service account with <code>owner</code>. <strong>The secret is returned once.</strong>',
      params: [['name', 'body', 'A label you will recognise later'], ['scopes', 'body', '<code>repo:&lt;pattern&gt;:&lt;action&gt;</code> or <code>system:&lt;area&gt;:&lt;action&gt;</code>; actions read, write, delete, admin'], ['expires_at', 'body', 'Required, RFC 3339, at most 366 days ahead'], ['owner', 'body', 'Service account name (needs <code>system:tokens:write</code>)'], ['allowed_cidrs', 'body', 'Optional networks the token works from'], ['read_only', 'body', 'Optional; limits the token to reading']],
      body: '{\n  "name": "ci-publisher",\n  "scopes": ["repo:docker-hosted:read", "repo:docker-hosted:write"],\n  "expires_at": "2027-02-01T00:00:00Z"\n}',
      status: '201 Created', res: '{\n  "id": "…",\n  "name": "ci-publisher",\n  "owner": "admin",\n  "prefix": "hgr_8Kq3",\n  "scopes": ["repo:docker-hosted:read", "repo:docker-hosted:write"],\n  "expires_at": "2027-02-01T00:00:00Z",\n  "allowed_cidrs": [],\n  "read_only": false,\n  "secret": "hgr_8Kq3…"\n}' }) +
    '<h2>List tokens</h2>' + api({ v: 'GET', p: '/tokens', ex: '/tokens', d: 'Your tokens, without secrets. With <code>system:tokens:read</code>, <code>?all=true</code> and <code>owner</code> list other people\'s.', res: '{\n  "items": [\n    { "id": "…", "name": "ci-publisher", "prefix": "hgr_8Kq3", "last_used_at": "2026-11-11T07:55:10Z", "expires_at": "2027-02-01T00:00:00Z" }\n  ],\n  "next_cursor": null\n}' }) +
    '<h2>Rotate a token</h2>' + api({ v: 'POST', p: '/tokens/{id}/rotate', ex: '/tokens/<id>/rotate', d: 'A new secret with the same name, scopes and lifetime. The old token keeps working for <code>revoke_old_after</code>.', body: '{\n  "revoke_old_after": "24h"\n}', res: '{\n  "id": "…",\n  "secret": "hgr_…",\n  "replaced": { "id": "…", "revoked": false, "expires_at": "…" }\n}' }) +
    '<h2>Revoke a token</h2>' + api({ v: 'DELETE', p: '/tokens/{id}', ex: '/tokens/<id>', d: 'Revokes the token.', status: '204 No Content' }) +
    '<h2>Users and service accounts</h2>' + api({ v: 'POST', p: '/users', ex: '/users', d: 'Creates a local member or a service account (needs <code>system:users:write</code>). <code>GET /users</code> lists them, filtered by <code>q</code>, <code>kind</code>, <code>role</code>, <code>status</code> or <code>source</code>.',
      body: '{\n  "username": "ci-publisher",\n  "kind": "service_account",\n  "roles": ["CI deployer"]\n}', status: '201 Created', res: '{\n  "id": "…",\n  "username": "ci-publisher",\n  "kind": "service_account",\n  "roles": ["CI deployer"]\n}' }) };

  P['api-system'] = { src: 'docs/operations/monitoring.md', title: 'System API', lead: 'Health, status and version. These need no token.', body:
    '<h2>Liveness</h2>' + api({ v: 'GET', p: '/healthz', ex: URL + '/healthz', anon: true, d: 'Answers 200 while the process runs. At the server root, outside <code>/api/v1</code>.', res: '{"status":"ok"}' }) +
    '<h2>Readiness</h2>' + api({ v: 'GET', p: '/readyz', ex: URL + '/readyz', anon: true, d: 'Answers 200 when the database, blob store and event bus answer, and 503 otherwise or while the node drains.', res: '{\n  "status": "ok",\n  "checks": {\n    "database": { "status": "ok", "latency_ms": 1 },\n    "blobstore": { "status": "ok", "latency_ms": 0 },\n    "eventbus": { "status": "ok", "latency_ms": 0 }\n  }\n}' }) +
    '<h2>Status</h2>' + api({ v: 'GET', p: '/api/v1/system/status', ex: '/system/status', anon: true, d: 'Overall state for dashboards and scripts. Always 200; read <code>state</code> (ok, degraded or down).',
      res: '{\n  "state": "ok",\n  "message": "All systems normal",\n  "version": "v0.1.8",\n  "instance_name": "hangar",\n  "base_url": "' + URL + '",\n  "registry_host": "' + H + '",\n  "nodes_up": 1,\n  "nodes_total": 1,\n  "dependencies": [\n    { "name": "database", "status": "ok", "latency_ms": 1 },\n    { "name": "blobstore", "status": "ok", "latency_ms": 0 },\n    { "name": "eventbus", "status": "ok", "latency_ms": 0 }\n  ]\n}' }) +
    '<h2>Version</h2>' + api({ v: 'GET', p: '/api/v1/system/version', ex: '/system/version', anon: true, d: 'Build information.', res: '{\n  "version": "v0.1.8",\n  "commit": "…",\n  "date": "…",\n  "go_version": "go1.27.3"\n}' }) +
    '<h2>Metrics</h2><p><code>/metrics</code> serves Prometheus metrics, all prefixed <code>hangar_</code>; protect it with <code>metrics.auth</code> or keep it internal. Per-node details: <code>GET /api/v1/system/nodes</code> and <code>/api/v1/system/health</code> (need <code>system:cluster:read</code>).</p>' };

  P.troubleshooting = { src: 'docs/deploy/troubleshooting.md', title: 'Troubleshooting', lead: 'The problems people hit most, and the fix for each. Start with /readyz, /api/v1/system/status and the logs.', body:
    '<h3>docker login: "http: server gave HTTP response to HTTPS client"</h3><p>Docker needs HTTPS for every registry except localhost. Put Hangar behind TLS (see <a href="#installation">Installation</a>), or for a test lab only, add the host to <code>insecure-registries</code> in <code>/etc/docker/daemon.json</code>.</p>' +
    '<h3>docker login loops or says "unauthorized" although the password is right</h3><p><code>server.base_url</code> is wrong: the token realm in the 401 points to an address the client cannot reach. Set it to the public HTTPS URL.</p>' +
    '<h3>Large uploads fail with 413, or after exactly 60 seconds</h3><p>Your reverse proxy limits body size or time. Remove the body limit, turn request buffering off, and measure timeouts between reads. See ' + guide('docs/deploy/reverse-proxy.md', 'reverse-proxy.md') + '.</p>' +
    '<h3>Downloads return 403 "quarantined"</h3><p>The version has an open critical finding in a repository that quarantines them. Upgrade the dependency, or waive the finding on the Vulnerabilities screen.</p>' +
    '<h3>Every client gets 429 at once</h3><p>All clients appear with the reverse proxy\'s address and share one rate limit. Set <code>server.trusted_proxies</code> to the proxy\'s address.</p>' +
    '<h3>A proxy repository shows offline or x509 errors</h3><p>Hangar cannot reach the remote. Check the outbound proxy (<strong>Settings → Proxies</strong>), <code>http.ca_file</code> for a private CA, and <code>http.allow_private_networks</code> for internal hosts. Cached packages keep working meanwhile.</p>' +
    '<h3>Saving a proxy with credentials answers 501</h3><p>No <code>HANGAR_SECRET_KEY</code>. Set it on every node and restart.</p>' +
    '<h3>Browser sign-in does not stick over plain HTTP</h3><p>Session cookies are <code>Secure</code>. Use HTTPS; for a lab on plain HTTP (not localhost) only, <code>auth.session.insecure: true</code>.</p>' +
    '<h3>Still stuck?</h3><p>Download the node\'s logs from <strong>Operations → System logs</strong> and open an <a href="' + GH + '/issues">issue on GitHub</a>.</p>' };

  P.faq = { src: 'docs/ROADMAP.md', title: 'FAQ', lead: 'Short answers to common questions.', body:
    '<h3>Is Hangar a fork of another registry?</h3><p>No. It is written from scratch in Go, from the public protocol specifications and the behaviour of real clients, and does not use code from other registry products.</p>' +
    '<h3>Can I move from another registry?</h3><p>Yes. The <code>repository_import</code> task copies your published packages from any registry that speaks the format\'s protocol into hosted repositories, and <code>proxy_prewarm</code> fills the proxy caches from lockfiles or from the old registry. See <a href="#migrate">Move to Hangar</a>.</p>' +
    '<h3>Does it work without internet?</h3><p>Yes. Hosted repositories need nothing outside, and a proxy with <code>offline: true</code> serves only what it has cached. The install script installs from an internal mirror (<code>--mirror</code>) or a copied archive (<code>--files-from</code>), and scanning can use mirrored advisory databases.</p>' +
    '<h3>Are there user or storage limits?</h3><p>Hangar has no user or seat limits and no activation key. Storage is limited by your disk or bucket.</p>' +
    '<h3>Which database?</h3><p>PostgreSQL 16 or newer. Hangar applies its own schema migrations. Other databases are not supported.</p>' +
    '<h3>What is not done yet?</h3><p>SAML sign-in, licence policies, SBOM download and edge replication are planned. The ' + guide('docs/ROADMAP.md', 'roadmap') + ' lists what is done, partial and planned.</p>' +
    '<h3>How do I report a security issue?</h3><p>Privately, with <strong>Report a vulnerability</strong> on the <a href="' + GH + '/security">repository\'s Security tab</a>, not in a public issue.</p>' };

  /* ---------- build nav + flat order ---------- */
  var ORDER = [], SECTION = {};
  NAV.forEach(function (g) { g[1].forEach(function (p) { ORDER.push(p[0]); SECTION[p[0]] = g[0]; }); });
  var side = document.getElementById('side');
  side.innerHTML = NAV.map(function (g) {
    return '<h4>' + g[0] + '</h4>' + g[1].map(function (p) { return '<a href="#' + p[0] + '" data-page="' + p[0] + '">' + p[1] + '</a>'; }).join('');
  }).join('');

  var article = document.getElementById('article');
  var toc = document.getElementById('toc');
  var tocObserver = null;

  function slug(s) { return s.toLowerCase().replace(/<[^>]+>/g, '').replace(/&\w+;/g, '').replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, ''); }

  function render(id, scrollTo) {
    if (!P[id]) id = 'introduction';
    var p = P[id], i = ORDER.indexOf(id);
    var prev = ORDER[i - 1], next = ORDER[i + 1];
    article.classList.remove('enter'); void article.offsetWidth;
    article.innerHTML = '<div class="crumb">' + SECTION[id] + '</div><h1>' + p.title + '</h1><p class="lead">' + p.lead + '</p>' + p.body +
      '<div class="edit-row"><a href="' + DOC + (p.src || 'docs/guides/README.md') + '">Full guide on GitHub</a><span>Found a mistake? <a href="' + GH + '/issues/new">Tell us</a>.</span></div>' +
      '<nav class="pager" aria-label="Previous and next">' +
      (prev ? '<a href="#' + prev + '"><small>← Previous</small><b>' + P[prev].title + '</b></a>' : '<span></span>') +
      (next ? '<a class="next" href="#' + next + '"><small>Next →</small><b>' + P[next].title + '</b></a>' : '') + '</nav>';
    article.classList.add('enter');
    document.title = p.title + ' · Hangar Docs';

    // heading anchors + toc
    var heads = Array.prototype.slice.call(article.querySelectorAll('h2, h3'));
    var seen = {};
    heads.forEach(function (h) {
      var s = slug(h.textContent); if (seen[s]) s += '-' + (++seen[s]); else seen[s] = 1;
      h.id = s;
      var a = document.createElement('a'); a.className = 'anchor'; a.href = '#' + id; a.dataset.sec = s; a.textContent = '#'; a.setAttribute('aria-label', 'Link to this section');
      h.appendChild(a);
    });
    toc.innerHTML = heads.length ? '<h4>On this page</h4>' + heads.map(function (h) {
      return '<a href="#' + id + '" data-sec="' + h.id + '"' + (h.tagName === 'H3' ? ' class="sub"' : '') + '>' + h.firstChild.textContent + '</a>';
    }).join('') : '';
    if (tocObserver) tocObserver.disconnect();
    if (heads.length && 'IntersectionObserver' in window) {
      tocObserver = new IntersectionObserver(function (entries) {
        entries.forEach(function (e) {
          if (e.isIntersecting) Array.prototype.forEach.call(toc.querySelectorAll('a'), function (a) { a.classList.toggle('on', a.dataset.sec === e.target.id); });
        });
      }, { rootMargin: '-70px 0px -70% 0px' });
      heads.forEach(function (h) { tocObserver.observe(h); });
    }

    Array.prototype.forEach.call(side.querySelectorAll('a'), function (a) {
      if (a.dataset.page === id) { a.setAttribute('aria-current', 'page'); if (a.scrollIntoView && side.scrollHeight > side.clientHeight) { var r = a.getBoundingClientRect(); if (r.top < 64 || r.bottom > innerHeight) a.scrollIntoView({ block: 'center' }); } }
      else a.removeAttribute('aria-current');
    });
    document.body.classList.remove('menu');

    var target = scrollTo && document.getElementById(scrollTo);
    if (target) target.scrollIntoView(); else window.scrollTo(0, 0);
  }

  function route() {
    var h = decodeURIComponent(location.hash.slice(1));
    var parts = h.split(':');               // #page or #page:section (stays a plain hash)
    render(parts[0], parts[1]);
  }
  window.addEventListener('hashchange', route);
  route();

  /* ---------- delegated clicks: copy, tabs, in-page section links ---------- */
  document.addEventListener('click', function (e) {
    var t = e.target;
    var cp = t.closest && t.closest('.code-copy');
    if (cp) {
      var pre = cp.closest('.code').querySelector('pre');
      var text = pre.innerText.split('\n').map(function (l) { return l.replace(/^\$ /, ''); }).join('\n');
      var done = function () { cp.textContent = 'Copied'; setTimeout(function () { cp.textContent = 'Copy'; }, 1400); };
      if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(text).then(done, function () { selectPre(pre, cp); });
      else selectPre(pre, cp);
      return;
    }
    var zm = t.closest && t.closest('.zoom');
    if (zm) {
      var lb = document.createElement('div'); lb.className = 'lightbox'; lb.setAttribute('role', 'dialog'); lb.setAttribute('aria-label', 'Screenshot');
      lb.innerHTML = '<img src="' + zm.querySelector('img').getAttribute('src') + '" alt="">';
      var close = function (ev) { if (!ev.key || ev.key === 'Escape') { lb.remove(); document.removeEventListener('keydown', close); } };
      lb.addEventListener('click', close);
      document.addEventListener('keydown', close);
      document.body.appendChild(lb);
      return;
    }
    var tab = t.closest && t.closest('.tabs [role="tab"]');
    if (tab) {
      var box = tab.closest('.tabs');
      Array.prototype.forEach.call(box.querySelectorAll('[role="tab"]'), function (b) { b.setAttribute('aria-selected', b === tab ? 'true' : 'false'); });
      Array.prototype.forEach.call(box.querySelectorAll('[role="tabpanel"]'), function (p) { p.hidden = p.id !== tab.getAttribute('aria-controls'); });
      return;
    }
    var sec = t.closest && t.closest('[data-sec]');
    if (sec) {
      e.preventDefault();
      var el = document.getElementById(sec.dataset.sec);
      if (el) { el.scrollIntoView({ behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' }); history.replaceState(null, '', '#' + location.hash.slice(1).split(':')[0] + ':' + sec.dataset.sec); }
    }
  });
  function selectPre(pre, btn) { var r = document.createRange(); r.selectNodeContents(pre); var s = getSelection(); s.removeAllRanges(); s.addRange(r); btn.textContent = 'Press ⌘C'; }

  /* ---------- reading progress ---------- */
  var ticking = false;
  window.addEventListener('scroll', function () {
    if (ticking) return; ticking = true;
    requestAnimationFrame(function () {
      ticking = false;
      var max = document.documentElement.scrollHeight - innerHeight;
      document.documentElement.style.setProperty('--read', max > 0 ? (scrollY / max).toFixed(4) : 0);
    });
  }, { passive: true });

  /* ---------- mobile menu ---------- */
  var menuBtn = document.getElementById('menu-btn');
  menuBtn.addEventListener('click', function () { var o = document.body.classList.toggle('menu'); menuBtn.setAttribute('aria-expanded', o); });
  document.getElementById('scrim').addEventListener('click', function () { document.body.classList.remove('menu'); menuBtn.setAttribute('aria-expanded', 'false'); });

  /* ---------- theme ---------- */
  var themeBtn = document.getElementById('theme-btn');
  if (themeBtn) themeBtn.addEventListener('click', function () {
    var cur = document.documentElement.getAttribute('data-theme') || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
    var nx = cur === 'dark' ? 'light' : 'dark';
    document.documentElement.setAttribute('data-theme', nx);
    try { localStorage.setItem('hangar-theme', nx); } catch (e) {}
  });

  /* ---------- search ---------- */
  var INDEX = [];
  ORDER.forEach(function (id) {
    var tmp = document.createElement('div'); tmp.innerHTML = P[id].body;
    INDEX.push({ id: id, sec: '', title: P[id].title, text: (P[id].lead + ' ' + tmp.textContent).replace(/\s+/g, ' ') });
    Array.prototype.forEach.call(tmp.querySelectorAll('h2, h3'), function (h) {
      var body = '', n = h.nextElementSibling;
      while (n && !/^H[23]$/.test(n.tagName)) { body += ' ' + n.textContent; n = n.nextElementSibling; }
      INDEX.push({ id: id, sec: slug(h.textContent), title: h.textContent, page: P[id].title, text: body.replace(/\s+/g, ' ') });
    });
  });
  var dlg = document.getElementById('search'), input = document.getElementById('search-input'), list = document.getElementById('results');
  var sel = 0, hits = [];
  function openSearch() { dlg.classList.add('open'); input.value = ''; find(''); input.focus(); }
  function closeSearch() { dlg.classList.remove('open'); }
  function mark(s, q) { if (!q) return esc(s); var i = s.toLowerCase().indexOf(q); return i < 0 ? esc(s) : esc(s.slice(0, i)) + '<mark>' + esc(s.slice(i, i + q.length)) + '</mark>' + esc(s.slice(i + q.length)); }
  function find(q) {
    q = q.trim().toLowerCase();
    if (!q) hits = INDEX.filter(function (x) { return !x.sec; }).slice(0, 8).map(function (x) { return { x: x, snip: '' }; });
    else {
      var words = q.split(/\s+/);
      hits = INDEX.map(function (x) {
        var t = x.title.toLowerCase(), b = x.text.toLowerCase(), score = 0;
        for (var i = 0; i < words.length; i++) {
          var w = words[i];
          if (t.indexOf(w) !== -1) score += t.indexOf(w) === 0 ? 12 : 8;
          else if (b.indexOf(w) !== -1) score += 2;
          else return null;
        }
        if (!x.sec) score += 1;
        var at = b.indexOf(words[0]);
        var snip = at < 0 ? x.text.slice(0, 110) : (at > 40 ? '…' : '') + x.text.slice(Math.max(0, at - 40), at + 80) + '…';
        return { x: x, score: score, snip: snip };
      }).filter(Boolean).sort(function (a, b) { return b.score - a.score; }).slice(0, 12);
    }
    sel = 0;
    list.innerHTML = hits.length ? hits.map(function (h, i) {
      var href = '#' + h.x.id + (h.x.sec ? ':' + h.x.sec : '');
      return '<li><a href="' + href + '" class="' + (i === 0 ? 'sel' : '') + '"><span class="sec">' + (h.x.sec ? h.x.page : SECTION[h.x.id]) + '</span><br>' + mark(h.x.title, q.split(/\s+/)[0]) + (h.snip ? '<small>' + mark(h.snip, q.split(/\s+/)[0]) + '</small>' : '') + '</a></li>';
    }).join('') : '<li class="empty">No results for “' + esc(q) + '”. Try “proxy”, “token” or “backup”.</li>';
  }
  function move(d) {
    var links = list.querySelectorAll('a'); if (!links.length) return;
    links[sel].classList.remove('sel'); sel = (sel + d + links.length) % links.length; links[sel].classList.add('sel'); links[sel].scrollIntoView({ block: 'nearest' });
  }
  input.addEventListener('input', function () { find(input.value); });
  input.addEventListener('keydown', function (e) {
    if (e.key === 'ArrowDown') { e.preventDefault(); move(1); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); move(-1); }
    else if (e.key === 'Enter') { var a = list.querySelectorAll('a')[sel]; if (a) { location.hash = a.getAttribute('href'); closeSearch(); } }
  });
  list.addEventListener('click', function (e) { if (e.target.closest('a')) closeSearch(); });
  dlg.addEventListener('click', function (e) { if (e.target === dlg) closeSearch(); });
  document.getElementById('search-btn').addEventListener('click', openSearch);
  document.addEventListener('keydown', function (e) {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); dlg.classList.contains('open') ? closeSearch() : openSearch(); }
    else if (e.key === '/' && !dlg.classList.contains('open') && !/INPUT|TEXTAREA/.test(document.activeElement.tagName)) { e.preventDefault(); openSearch(); }
    else if (e.key === 'Escape' && dlg.classList.contains('open')) closeSearch();
  });
})();
