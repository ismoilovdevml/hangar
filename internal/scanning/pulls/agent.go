package pulls

import "strings"

// agentRules map a lower-case user-agent substring (or, for short product
// names, prefix) to the class shown for it, most specific first.
var agentRules = []struct {
	match, class string
	prefix       bool
}{
	{"nuget", "NuGet", false},
	{"dotnet", "NuGet", false},
	{"msbuild", "NuGet", false},
	{"pnpm/", "pnpm", true},
	{"yarn/", "Yarn", true},
	{"bun/", "Bun", true},
	{"npm/", "npm", true},
	{"uv/", "uv", true},
	{"poetry/", "Poetry", true},
	{"pip/", "pip", true},
	{"twine/", "twine", true},
	{"apache-maven", "Maven", false},
	{"maven", "Maven", false},
	{"gradle", "Gradle", false},
	{"sbt/", "sbt", true},
	{"go-http-client", "Go", false},
	{"go/", "Go", true},
	{"buildkit", "BuildKit", false},
	{"containerd", "containerd", false},
	{"docker", "Docker", false},
	{"podman", "Podman", false},
	{"skopeo", "skopeo", false},
	{"curl/", "curl", true},
	{"wget", "Wget", false},
	{"python-requests", "Python", false},
	{"python-urllib", "Python", false},
	{"aiohttp", "Python", false},
	{"java/", "Java", true},
	{"okhttp", "Java", false},
	{"mozilla/", "Browser", false},
}

// AgentClass reduces a User-Agent header to the kind of client that sent it
// ("NuGet", "npm", "pip", "Maven", "Browser", ...), without versions or
// operating system details. Unknown agents keep their product name, at most
// 32 characters; an empty header is "Unknown client".
func AgentClass(ua string) string {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return "Unknown client"
	}
	l := strings.ToLower(ua)
	for _, r := range agentRules {
		if r.prefix && strings.HasPrefix(l, r.match) || !r.prefix && strings.Contains(l, r.match) {
			return r.class
		}
	}
	product := ua
	if i := strings.IndexAny(product, "/ ("); i > 0 {
		product = product[:i]
	}
	product = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, product)
	if len(product) > 32 {
		product = product[:32]
	}
	if product == "" {
		return "Unknown client"
	}
	return product
}
