package npm

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/routing"
	"github.com/ismoilovdevml/hangar/internal/scanning"
)

// npm audit. npm asks POST /-/npm/v1/security/advisories/bulk with
// {"<package>": ["<version>", ...]} and expects, per package, the advisories
// that might apply ({"id", "url", "title", "severity",
// "vulnerable_versions", ...}); it matches the installed versions against
// vulnerable_versions itself. When that fails it falls back to the older
// /-/npm/v1/security/audits/quick (and npm 6 uses /audits), which send the
// lockfile tree and expect a full report.
//
// Hangar answers every form from the same plan:
//
//   - packages a proxy repository may fetch (its own and its group's
//     routing rules) are asked of the proxy's remote, through its bulk
//     endpoint, with only those package names in the body;
//   - packages stored in a hosted repository are answered from Hangar's own
//     stored vulnerability findings (OSV scanning) of those versions.
//
// Nothing is ever answered with a guess. When a hosted version that the
// request names has not been scanned successfully, or scanning is off, or a
// remote cannot answer, the request fails (503 or 502) and npm reports that
// the audit could not be done, instead of "found 0 vulnerabilities".

// Advisories is the vulnerability scanner as npm audit reads it
// (implemented by *scanning.Service).
type Advisories interface {
	Enabled() bool
	VersionFindings(ctx context.Context, repoIDs []string, keys []scanning.ComponentKey) ([]*scanning.VersionReport, error)
}

type advisoriesBox struct{ a Advisories }

// SetAdvisories gives npm audit the scanner's stored findings. Without it
// (or with scanning off) audits of hosted packages fail with 503.
func (f *Format) SetAdvisories(a Advisories) { f.advisories.Store(&advisoriesBox{a: a}) }

func (f *Format) advisoryDB() Advisories {
	if b := f.advisories.Load(); b != nil {
		return b.a
	}
	return nil
}


// errAuditUnavailable answers 503: Hangar cannot vouch for the answer.
var errAuditUnavailable = errors.New("npm audit unavailable")

// Request limits.
const (
	maxAuditBody     = 64 << 20
	maxAuditPackages = 20000
	maxAuditPaths    = 10 // dependency paths kept per version (quick audits)
	bulkAuditPath    = "-/npm/v1/security/advisories/bulk"
)

// auditRequest is what an audit asks about: package -> versions, plus the
// dependency paths of each name@version for the full report.
type auditRequest struct {
	pkgs  map[string][]string
	paths map[string][]string
}

func (q *auditRequest) add(name, version, path string) {
	if _, err := parseName(name); err != nil || version == "" || len(q.pkgs) >= maxAuditPackages && q.pkgs[name] == nil {
		return
	}
	if !slices.Contains(q.pkgs[name], version) {
		q.pkgs[name] = append(q.pkgs[name], version)
	}
	if path != "" {
		k := name + "@" + version
		if len(q.paths[k]) < maxAuditPaths {
			q.paths[k] = append(q.paths[k], path)
		}
	}
}

func (h *handler) audit(w http.ResponseWriter, r *http.Request, repo *core.Repository, rest string) error {
	if r.Method != http.MethodPost {
		return errMethod
	}
	ctx := r.Context()
	if err := h.authorize(ctx, core.ActionRead, repo); err != nil {
		return err
	}
	full := rest == "audits/quick" || rest == "audits"
	if rest != "advisories/bulk" && !full {
		return fmt.Errorf("unknown audit endpoint: %w", core.ErrNotFound)
	}
	body, err := readAuditBody(r)
	if err != nil {
		return err
	}
		q := &auditRequest{pkgs: map[string][]string{}, paths: map[string][]string{}}
	if full {
		err = parseAuditTree(body, q)
	} else {
		err = parseBulk(body, q)
	}
	if err != nil {
		return err
	}
	res, err := h.auditRepo(ctx, repo, q.pkgs)
	if err != nil {
		return err
	}
	if full {
		writeJSON(w, http.StatusOK, res.report(q))
		return nil
	}
	writeJSON(w, http.StatusOK, res.bulk())
	return nil
}

// readAuditBody reads the (possibly gzip-encoded, as npm sends it) body.
func readAuditBody(r *http.Request) ([]byte, error) {
	var rd io.Reader = io.LimitReader(r.Body, maxAuditBody)
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Content-Encoding")), "gzip") {
		gz, err := gzip.NewReader(rd)
		if err != nil {
			return nil, fmt.Errorf("audit body is not gzip: %w", core.ErrInvalid)
		}
		defer func() { _ = gz.Close() }()
		rd = gz
	}
	b, err := io.ReadAll(io.LimitReader(rd, maxAuditBody+1))
	if err != nil {
		return nil, fmt.Errorf("read audit body: %w: %w", err, core.ErrInvalid)
	}
	if len(b) > maxAuditBody {
		return nil, fmt.Errorf("audit body exceeds %d bytes: %w", maxAuditBody, errTooLarge)
	}
	return b, nil
}

func parseBulk(body []byte, q *auditRequest) error {
	var m map[string][]string
	if err := json.Unmarshal(body, &m); err != nil {
		return fmt.Errorf("bulk audit body: %w: %w", err, core.ErrInvalid)
	}
	for name, vs := range m {
		for _, v := range vs {
			q.add(name, v, "")
		}
	}
	return nil
}

// auditNode is one entry of the lockfile tree a quick audit sends.
type auditNode struct {
	Version      string                `json:"version"`
	Dependencies map[string]*auditNode `json:"dependencies"`
}

func parseAuditTree(body []byte, q *auditRequest) error {
	var top auditNode
	if err := json.Unmarshal(body, &top); err != nil {
		return fmt.Errorf("audit body: %w: %w", err, core.ErrInvalid)
	}
	var walk func(deps map[string]*auditNode, prefix string, depth int)
	walk = func(deps map[string]*auditNode, prefix string, depth int) {
		if depth > 64 {
			return
		}
		for name, n := range deps {
			if n == nil {
				continue
			}
			path := name
			if prefix != "" {
				path = prefix + ">" + name
			}
			q.add(name, n.Version, path)
			walk(n.Dependencies, path, depth+1)
		}
	}
	walk(top.Dependencies, "", 0)
	return nil
}

// auditPlan says who answers for which package versions.
type auditPlan struct {
	hosted  map[string]*core.Repository     // by id
	hostedQ map[string]map[string][]string // repo id -> package -> versions
	proxies map[string]*core.Repository     // by id
	proxyQ  map[string]map[string][]string
}

func addAll(dst map[string]map[string][]string, id string, pkgs map[string][]string) {
	m := dst[id]
	if m == nil {
		m = map[string][]string{}
		dst[id] = m
	}
	for n, vs := range pkgs {
		for _, v := range vs {
			if !slices.Contains(m[n], v) {
				m[n] = append(m[n], v)
			}
		}
	}
}

func (h *handler) plan(ctx context.Context, repo *core.Repository, pkgs map[string][]string, depth int, p *auditPlan) error {
	if len(pkgs) == 0 {
		return nil
	}
	switch repo.Type {
	case core.RepoHosted:
		p.hosted[repo.ID] = repo
		addAll(p.hostedQ, repo.ID, pkgs)
	case core.RepoProxy:
		allowed := map[string][]string{}
		for n, vs := range pkgs {
			if routing.Check(ctx, repo, n) == nil {
				allowed[n] = vs
			}
		}
		if len(allowed) > 0 {
			p.proxies[repo.ID] = repo
			addAll(p.proxyQ, repo.ID, allowed)
		}
	case core.RepoGroup:
		if depth >= maxGroupDepth {
			return fmt.Errorf("group %q: nesting deeper than %d: %w", repo.Name, maxGroupDepth, core.ErrInvalid)
		}
		members, err := h.deps.Repos.Members(ctx, repo)
		if err != nil {
			return err
		}
		for _, m := range members {
			if m.Format != Name || !m.Online {
				continue
			}
			sub := map[string][]string{}
			for n, vs := range pkgs {
				if routing.Allows(ctx, repo, m, n) {
					sub[n] = vs
				}
			}
			if err := h.plan(ctx, m, sub, depth+1, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// bulkAdvisory is one advisory of a bulk answer; raw is what is sent on.
type bulkAdvisory struct {
	raw       json.RawMessage
	id        string
	url       string
	title     string
	severity  string
	versions  string // vulnerable_versions
	installed []string
}

// auditResult collects advisories per package, without duplicates.
type auditResult struct {
	by   map[string][]*bulkAdvisory
	seen map[string]bool
}

func (a *auditResult) add(name string, adv *bulkAdvisory) {
	key := name + "\x00" + adv.url
	if adv.url == "" {
		key = name + "\x00id:" + adv.id
	}
	if a.seen[key] {
		return
	}
	a.seen[key] = true
	a.by[name] = append(a.by[name], adv)
}

func (a *auditResult) bulk() map[string][]json.RawMessage {
	out := make(map[string][]json.RawMessage, len(a.by))
	for n, list := range a.by {
		for _, adv := range list {
			out[n] = append(out[n], adv.raw)
		}
	}
	return out
}

// auditRepo answers an audit of pkgs addressed to repo.
func (h *handler) auditRepo(ctx context.Context, repo *core.Repository, pkgs map[string][]string) (*auditResult, error) {
	p := &auditPlan{hosted: map[string]*core.Repository{}, hostedQ: map[string]map[string][]string{},
		proxies: map[string]*core.Repository{}, proxyQ: map[string]map[string][]string{}}
	if err := h.plan(ctx, repo, pkgs, 0, p); err != nil {
		return nil, err
	}
	res := &auditResult{by: map[string][]*bulkAdvisory{}, seen: map[string]bool{}}
	if err := h.auditHosted(ctx, p, res); err != nil {
		return nil, err
	}
	ids := slices.Sorted(maps.Keys(p.proxies))
	for _, id := range ids {
		if err := h.auditRemote(ctx, p.proxies[id], p.proxyQ[id], res); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// auditRemote asks a proxy's remote about pkgs (names its rules allow).
func (h *handler) auditRemote(ctx context.Context, repo *core.Repository, pkgs map[string][]string, res *auditResult) error {
	body, err := json.Marshal(pkgs)
	if err != nil {
		return err
	}
	up, err := h.deps.Upstreams.ForRepository(ctx, repo)
	if err != nil {
		return err
	}
	resp, err := up.Fetch(ctx, core.UpstreamRequest{Method: http.MethodPost, Path: bulkAuditPath, Body: body, Index: true,
		Header: http.Header{"Content-Type": {mediaJSON}, "Accept": {mediaJSON}}})
	if err != nil {
		return fmt.Errorf("audit through %s: %w", repo.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the remote of %s answered %d to the audit, so it cannot be completed: %w", repo.Name, resp.StatusCode, core.ErrUpstream)
	}
	var doc map[string][]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAuditBody)).Decode(&doc); err != nil {
		return fmt.Errorf("the remote of %s answered the audit with invalid JSON: %w", repo.Name, core.ErrUpstream)
	}
	for name, list := range doc {
		if _, asked := pkgs[name]; !asked {
			continue
		}
		for _, raw := range list {
			var f struct {
				ID       any    `json:"id"`
				URL      string `json:"url"`
				Title    string `json:"title"`
				Severity string `json:"severity"`
				Versions string `json:"vulnerable_versions"`
			}
			if json.Unmarshal(raw, &f) != nil {
				continue
			}
			res.add(name, &bulkAdvisory{raw: raw, id: fmt.Sprint(f.ID), url: f.URL, title: f.Title,
				severity: f.Severity, versions: f.Versions})
		}
	}
	return nil
}

// auditHosted answers for the versions stored in hosted repositories from
// their stored findings.
func (h *handler) auditHosted(ctx context.Context, p *auditPlan, res *auditResult) error {
	if len(p.hostedQ) == 0 {
		return nil
	}
	adv := h.f.advisoryDB()
	if adv == nil || !adv.Enabled() {
		// Only a version Hangar actually stores needs an answer from it.
		for id, pkgs := range p.hostedQ {
			for name, vs := range pkgs {
				n, _ := parseName(name)
				comps, err := h.listVersions(ctx, p.hosted[id], n)
				if err != nil {
					return err
				}
				for _, c := range comps {
					if slices.Contains(vs, c.Version) {
						return fmt.Errorf("%s@%s is stored in %s, and vulnerability scanning is turned off, so Hangar cannot audit it: %w",
							name, c.Version, p.hosted[id].Name, errAuditUnavailable)
					}
				}
			}
		}
		return nil
	}
	var keys []scanning.ComponentKey
	for _, pkgs := range p.hostedQ {
		for name, vs := range pkgs {
			n, _ := parseName(name)
			for _, v := range vs {
				keys = append(keys, scanning.ComponentKey{Namespace: n.Scope, Name: n.Base, Version: v})
			}
		}
	}
	reports, err := adv.VersionFindings(ctx, slices.Sorted(maps.Keys(p.hostedQ)), keys)
	if err != nil {
		return err
	}
	var pending []string
	type group struct {
		f         *scanning.Finding
		installed []string
	}
	groups := map[string]*group{}
	var order []string
	for _, rep := range reports {
		name := rep.Name
		if rep.Namespace != "" {
			name = rep.Namespace + "/" + rep.Name
		}
		if !slices.Contains(p.hostedQ[rep.RepositoryID][name], rep.Version) {
			continue
		}
		if rep.ScanStatus != "ok" {
			why := "not scanned yet"
			if rep.ScanStatus != "" {
				why = "last scan " + rep.ScanStatus
			}
			pending = append(pending, name+"@"+rep.Version+" ("+why+")")
			continue
		}
		for _, f := range rep.Findings {
			if f.Status == "waived" {
				continue // an accepted risk in Hangar
			}
			k := name + "\x00" + f.AdvisoryID
			g := groups[k]
			if g == nil {
				g = &group{f: f}
				groups[k] = g
				order = append(order, k)
			}
			if !slices.Contains(g.installed, rep.Version) {
				g.installed = append(g.installed, rep.Version)
			}
		}
	}
	if len(pending) > 0 {
		sort.Strings(pending)
		more := ""
		if len(pending) > 5 {
			more = fmt.Sprintf(" and %d more", len(pending)-5)
			pending = pending[:5]
		}
		return fmt.Errorf("Hangar has no vulnerability scan result for %s%s; scan the repository or try again later: %w",
			strings.Join(pending, ", "), more, errAuditUnavailable)
	}
	sort.Strings(order)
	for _, k := range order {
		g := groups[k]
		name, _, _ := strings.Cut(k, "\x00")
		a, err := findingAdvisory(g.f, g.installed)
		if err != nil {
			return err
		}
		res.add(name, a)
	}
	return nil
}

// findingAdvisory renders a stored finding as a bulk advisory.
func findingAdvisory(f *scanning.Finding, installed []string) (*bulkAdvisory, error) {
	sort.Strings(installed)
	vv := npmRange(f.AffectedVersions)
	for _, v := range installed {
		if ok, known := satisfies(v, vv); !known || !ok {
			vv = joinRange(vv, v) // the scan found this version affected
		}
	}
	a := &bulkAdvisory{id: strconv.FormatInt(advisoryNumber(f.AdvisoryID), 10), url: advisoryURL(f), title: f.Title, severity: npmSeverity(f.Severity),
		versions: vv, installed: installed}
	if a.title == "" {
		a.title = f.AdvisoryID
	}
	doc := map[string]any{"id": advisoryNumber(f.AdvisoryID), "url": a.url, "title": a.title, "severity": a.severity,
		"vulnerable_versions": a.versions, "cwe": []string{}}
	if f.CVSS != nil {
		cv := map[string]any{"score": *f.CVSS}
		if f.CVSSVector != nil {
			cv["vectorString"] = *f.CVSSVector
		}
		doc["cvss"] = cv
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	a.raw = raw
	return a, nil
}

// advisoryNumber is the numeric id clients key advisories by (pnpm skips
// advisories whose id is not a number): a stable hash of the advisory id,
// within JavaScript's safe integers. The advisory itself is named by url.
func advisoryNumber(id string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	return int64(h.Sum64() & (1<<53 - 1)) //nolint:gosec // masked to 53 bits
}

func joinRange(r, v string) string {
	if r == "" {
		return v
	}
	return r + " || " + v
}

// npmRange turns the stored affected ranges (">=1.0.0, <1.2.3; <0.9.1",
// "all versions") into npm range syntax.
func npmRange(affected string) string {
	var sets []string
	for _, part := range strings.Split(affected, ";") {
		part = strings.TrimSpace(part)
		switch {
		case part == "":
			continue
		case part == "all versions":
			return "*"
		}
		sets = append(sets, strings.Join(strings.Fields(strings.ReplaceAll(part, ",", " ")), " "))
	}
	return strings.Join(sets, " || ")
}

// advisoryURL is the advisory's page: its ADVISORY reference, the GitHub
// advisory of a GHSA id (the URL npm's registry uses, so the same advisory
// from a remote and from Hangar is shown once), or the OSV page.
func advisoryURL(f *scanning.Finding) string {
	for _, r := range f.References {
		if r.Type == "ADVISORY" && strings.HasPrefix(r.URL, "https://github.com/advisories/") {
			return r.URL
		}
	}
	for _, id := range append([]string{f.AdvisoryID}, f.Aliases...) {
		if strings.HasPrefix(id, "GHSA-") {
			return "https://github.com/advisories/" + id
		}
	}
	return "https://osv.dev/vulnerability/" + f.AdvisoryID
}

// npmSeverity maps Hangar's levels to npm's. Advisories without a rating
// count as high, as npm itself treats advisories without a severity.
func npmSeverity(s string) string {
	switch s {
	case "critical", "high", "low":
		return s
	case "medium":
		return "moderate"
	}
	return "high"
}

// report renders the full audit report of /audits and /audits/quick.
func (a *auditResult) report(q *auditRequest) map[string]any {
	counts := map[string]int{"info": 0, "low": 0, "moderate": 0, "high": 0, "critical": 0}
	advisories := map[string]any{}
	names := slices.Sorted(maps.Keys(a.by))
	n := 0
	for _, name := range names {
		for _, adv := range a.by[name] {
			n++
			var findings []map[string]any
			for _, v := range q.pkgs[name] {
				if ok, known := satisfies(v, adv.versions); ok || !known {
					paths := q.paths[name+"@"+v]
					if paths == nil {
						paths = []string{name}
					}
					findings = append(findings, map[string]any{"version": v, "paths": paths})
					sev := adv.severity
					if _, ok := counts[sev]; !ok {
						sev = "high"
					}
					counts[sev] += len(paths)
				}
			}
			if findings == nil {
				continue
			}
			id := adv.id
			if id == "" || id == "<nil>" {
				id = strconv.Itoa(n)
			}
			advisories[id] = map[string]any{
				"id": adv.id, "module_name": name, "title": adv.title, "overview": adv.title, "severity": adv.severity,
				"vulnerable_versions": adv.versions, "url": adv.url, "findings": findings,
				"recommendation": "Upgrade to a version outside " + adv.versions, "patched_versions": "", "cwe": []string{},
			}
		}
	}
	deps := 0
	for _, vs := range q.pkgs {
		deps += len(vs)
	}
	return map[string]any{
		"actions": []any{}, "advisories": advisories, "muted": []any{},
		"metadata": map[string]any{"vulnerabilities": counts, "dependencies": deps, "devDependencies": 0,
			"optionalDependencies": 0, "totalDependencies": deps},
	}
}

// satisfies evaluates an npm range made of comparators (">=1.0.0 <1.2.3",
// "<4.17.21", "1.2.3", "*") joined by "||". known is false for syntax it
// does not evaluate (caret, tilde, hyphen ranges), which callers treat as
// affected rather than guess.
func satisfies(version, rng string) (ok, known bool) {
	v, err := parseSemver(version)
	if err != nil {
		return false, false
	}
	for _, set := range strings.Split(rng, "||") {
		toks := strings.Fields(set)
		// ">= 1.0.0" is ">=1.0.0".
		var comps []string
		for i := 0; i < len(toks); i++ {
			t := toks[i]
			if strings.Trim(t, "<>=") == "" && i+1 < len(toks) {
				t += toks[i+1]
				i++
			}
			comps = append(comps, t)
		}
		all := true
		for _, c := range comps {
			m, k := comparatorMatches(v, c)
			if !k {
				return false, false
			}
			if !m {
				all = false
			}
		}
		if all {
			return true, true
		}
	}
	return false, true
}

func comparatorMatches(v semver, c string) (bool, bool) {
	if c == "*" || c == "x" || c == "" {
		return true, true
	}
	op := strings.TrimRight(c[:len(c)-len(strings.TrimLeft(c, "<>="))], " ")
	rest := strings.TrimPrefix(strings.TrimLeft(c, "<>="), "v")
	parts := strings.SplitN(strings.SplitN(rest, "-", 2)[0], ".", 3)
	for len(parts) < 3 {
		parts = append(parts, "0")
	}
	full := strings.Join(parts, ".")
	if i := strings.Index(rest, "-"); i >= 0 {
		full += rest[i:]
	}
	w, err := parseSemver(full)
	if err != nil {
		return false, false
	}
	cmp := v.compare(w)
	switch op {
	case "", "=":
		return cmp == 0, true
	case ">":
		return cmp > 0, true
	case ">=":
		return cmp >= 0, true
	case "<":
		return cmp < 0, true
	case "<=":
		return cmp <= 0, true
	}
	return false, false
}

