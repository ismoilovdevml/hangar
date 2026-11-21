package cac

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Specs are handled in the JSON model the REST API speaks: map[string]any,
// []any, string, bool, json.Number and nil. Comparison follows the apply
// semantics documented in docs/guides/config-as-code.md:
//
//   - a key the file leaves out is not managed: the server's value stays,
//     whatever it is (merge on update, ignored by the diff);
//   - a key the file sets to its zero value ("", false, 0, [], {}) equals a
//     key the server leaves out, as the API omits empty fields;
//   - null equals absent; anything else set to null is a change;
//   - numbers compare by value and Go durations by length ("24h" = "24h0m0s");
//   - lists compare element by element, or as sets where the API sorts them;
//   - objects marked atomic compare exactly and are sent as a whole.

// toJSON converts v (decoded from YAML, or any JSON-encodable value) into
// the JSON model.
func toJSON(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return decodeJSON(b)
}

func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// FieldDiff is one difference between the declared and the live value.
type FieldDiff struct {
	Path Path
	// Old is the live value (OldSet false when the server has none).
	Old    any
	OldSet bool
	// New is the declared value.
	New any
}

// differ collects the differences of one resource.
type differ struct {
	k   *kind
	out []FieldDiff
}

func (d *differ) add(p Path, have any, haveSet bool, want any) {
	d.out = append(d.out, FieldDiff{Path: p, Old: have, OldSet: haveSet && have != nil, New: want})
}

// diffSpec returns where want (the declared spec) differs from have (the
// live spec), in path order.
func (k *kind) diffSpec(want, have map[string]any) []FieldDiff {
	d := &differ{k: k}
	d.compare(nil, want, have, true)
	return d.out
}

func (d *differ) compare(p Path, want, have any, haveSet bool) {
	if d.k.isWriteOnly(p) {
		return
	}
	absent := !haveSet || have == nil
	switch w := want.(type) {
	case map[string]any:
		h, ok := have.(map[string]any)
		switch {
		case absent:
			if !isZero(w) {
				d.add(p, have, haveSet, want)
			}
		case !ok:
			d.add(p, have, haveSet, want)
		case d.k.isAtomic(p):
			if !d.exactEqual(p, w, h) {
				d.add(p, have, haveSet, want)
			}
		default:
			for _, key := range slices.Sorted(maps.Keys(w)) {
				hv, hok := h[key]
				d.compare(p.child(key), w[key], hv, hok)
			}
		}
	case []any:
		h, ok := have.([]any)
		switch {
		case absent:
			if len(w) > 0 {
				d.add(p, have, haveSet, want)
			}
		case !ok || !d.listEqual(p, w, h):
			d.add(p, have, haveSet, want)
		}
	case nil:
		if !absent {
			d.add(p, have, haveSet, want)
		}
	default:
		switch {
		case absent:
			if !isZero(want) {
				d.add(p, have, haveSet, want)
			}
		case !d.scalarEqual(p, want, have):
			d.add(p, have, haveSet, want)
		}
	}
}

// equal reports whether want, declared at p, matches have.
func (d *differ) equal(p Path, want, have any, haveSet bool) bool {
	sub := &differ{k: d.k}
	sub.compare(p, want, have, haveSet)
	return len(sub.out) == 0
}

// exactEqual compares two objects key by key in both directions: a key
// only the server has counts unless its value is zero. Write-only keys are
// skipped.
func (d *differ) exactEqual(p Path, w, h map[string]any) bool {
	for _, key := range slices.Sorted(maps.Keys(w)) {
		hv, ok := h[key]
		if !d.equal(p.child(key), w[key], hv, ok) {
			return false
		}
	}
	for key, hv := range h {
		if _, ok := w[key]; !ok && !d.k.isWriteOnly(p.child(key)) && !isZero(hv) {
			return false
		}
	}
	return true
}

func (d *differ) listEqual(p Path, w, h []any) bool {
	if len(w) != len(h) {
		return false
	}
	if d.k.isUnordered(p) {
		w, h = sortedByJSON(w), sortedByJSON(h)
	}
	item := p.child("[]")
	for i := range w {
		if !d.equal(item, w[i], h[i], true) {
			return false
		}
	}
	return true
}

func (d *differ) scalarEqual(p Path, want, have any) bool {
	switch w := want.(type) {
	case json.Number:
		h, ok := have.(json.Number)
		return ok && numberEqual(w, h)
	case string:
		h, ok := have.(string)
		if !ok {
			return false
		}
		if d.k.isURLSecret(p) {
			w = core.RedactURL(w)
		}
		return w == h || durationEqual(w, h)
	case bool:
		h, ok := have.(bool)
		return ok && w == h
	}
	return false
}

func numberEqual(a, b json.Number) bool {
	x, ok1 := new(big.Rat).SetString(string(a))
	y, ok2 := new(big.Rat).SetString(string(b))
	return ok1 && ok2 && x.Cmp(y) == 0
}

// durationEqual reports whether a and b are Go durations of the same
// length: the API answers "24h0m0s" for "24h".
func durationEqual(a, b string) bool {
	x, err1 := time.ParseDuration(a)
	y, err2 := time.ParseDuration(b)
	return err1 == nil && err2 == nil && x == y
}

// isZero reports whether v is what the API leaves out: null, "", false, 0,
// an empty list, or an object of zero values.
func isZero(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case json.Number:
		return numberEqual(x, "0")
	case []any:
		return len(x) == 0
	case map[string]any:
		for _, e := range x {
			if !isZero(e) {
				return false
			}
		}
		return true
	}
	return false
}

func sortedByJSON(xs []any) []any {
	type keyed struct {
		key string
		v   any
	}
	ks := make([]keyed, 0, len(xs))
	for _, x := range xs {
		b, _ := json.Marshal(x)
		ks = append(ks, keyed{string(b), x})
	}
	slices.SortStableFunc(ks, func(a, b keyed) int {
		switch {
		case a.key < b.key:
			return -1
		case a.key > b.key:
			return 1
		}
		return 0
	})
	out := make([]any, 0, len(ks))
	for _, k := range ks {
		out = append(out, k.v)
	}
	return out
}

// merge returns the body of an update: the live value with every key the
// file declares replaced by the declared value, objects merged key by key
// (atomic objects replaced as a whole). Keys the file leaves out keep the
// server's value, which is what the diff assumed.
func (k *kind) merge(p Path, live, want any) any {
	w, wok := want.(map[string]any)
	l, lok := live.(map[string]any)
	if !wok || !lok || k.isAtomic(p) {
		return want
	}
	out := maps.Clone(l)
	for key, wv := range w {
		out[key] = k.merge(p.child(key), l[key], wv)
	}
	return out
}

// project keeps the input fields of a live object: the part of a GET answer
// that a create or update sends.
func (k *kind) project(live map[string]any) map[string]any {
	out := map[string]any{}
	for _, f := range k.fields {
		if v, ok := live[f]; ok {
			out[f] = v
		}
	}
	return out
}

// secretText stands in for secret values in plans and messages.
const secretText = "(secret)"

// mask returns v, found at p, with every secret replaced by secretText:
// write-only fields, values that came from a secret reference (secrets)
// and passwords in URLs.
func (k *kind) mask(p Path, v any, secrets []Path) any {
	if k.isWriteOnly(p) || slices.ContainsFunc(secrets, p.equal) {
		if isZero(v) {
			return v
		}
		return secretText
	}
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for key, e := range x {
			out[key] = k.mask(p.child(key), e, secrets)
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			out = append(out, k.mask(p.child("[]"), e, secrets))
		}
		return out
	case string:
		if k.isURLSecret(p) {
			return core.RedactURL(x)
		}
	}
	return v
}

// render formats a value for a plan line: JSON, shortened.
func render(v any, set bool) string {
	if !set {
		return "(unset)"
	}
	if s, ok := v.(string); ok && s == secretText {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	const limit = 120
	if len(b) > limit {
		return string(b[:limit-3]) + "..."
	}
	return string(b)
}

// getPath returns the value at p inside v.
func getPath(v any, p Path) (any, bool) {
	for _, key := range p {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = m[key]; !ok {
			return nil, false
		}
	}
	return v, true
}

// setPath stores x at p inside m, creating objects on the way.
func setPath(m map[string]any, p Path, x any) {
	for _, key := range p[:len(p)-1] {
		next, ok := m[key].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[key] = next
		}
		m = next
	}
	m[p[len(p)-1]] = x
}
