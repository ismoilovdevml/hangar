package rpm

import (
	"crypto/sha256"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// pinWindow is how long after serving a client repomd.xml its signature
// request is answered for that same document.
const pinWindow = 2 * time.Minute

// maxPins bounds the remembered clients; the table is cleared when full.
const maxPins = 8192

// repomdPins closes the race between the two requests dnf makes for
// signed repodata: repomd.xml, then repomd.xml.asc. When an upload
// replaces repomd.xml between them, a signature of the new document would
// not verify the one the client holds. So the server remembers, per
// client, which repomd.xml it served, and signs that one when the same
// client asks for the signature shortly after, provided the document was
// replaced after it was served. Requests on another node, or from a client
// it has not seen, get the signature of the current document.
type repomdPins struct {
	mu sync.Mutex
	m  map[string]pin
}

type pin struct {
	sum [32]byte
	doc []byte
	at  time.Time
}

// pinKey identifies a client of a repodata root: principal, address and
// user agent.
func pinKey(r *http.Request, repo *core.Repository, root string) string {
	ctx := r.Context()
	info := core.RequestInfoFrom(ctx)
	ip := info.RemoteIP
	if ip == "" {
		ip, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	ua := info.UserAgent
	if ua == "" {
		ua = r.UserAgent()
	}
	return repo.ID + "\x00" + root + "\x00" + core.PrincipalFrom(ctx).Name + "\x00" + ip + "\x00" + ua
}

// served records that key was given doc at time at.
func (p *repomdPins) served(key string, doc []byte, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil || len(p.m) >= maxPins {
		p.m = map[string]pin{}
	}
	p.m[key] = pin{sum: sha256.Sum256(doc), doc: doc, at: at}
}

// document returns the repomd.xml to sign for key: the one it was served,
// when that was within pinWindow and before current replaced it (at
// replaced), else current.
func (p *repomdPins) document(key string, current []byte, replaced, now time.Time) []byte {
	p.mu.Lock()
	pn, ok := p.m[key]
	p.mu.Unlock()
	if !ok || now.Sub(pn.at) > pinWindow || pn.sum == sha256.Sum256(current) || !pn.at.Before(replaced) {
		return current
	}
	return pn.doc
}
