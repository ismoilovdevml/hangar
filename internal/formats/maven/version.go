package maven

import (
	"sort"
	"strconv"
	"strings"
)

// Version ordering follows the Maven "Version Order Specification"
// (https://maven.apache.org/pom.html#Version_Order_Specification):
// versions are split into numeric and qualifier tokens at '.', '-' and at
// digit/letter transitions; trailing "null" tokens (0, "", final, ga, release)
// are dropped; qualifiers order alpha < beta < milestone < rc = cr <
// snapshot < "" = final = ga = release < sp, unknown qualifiers after those,
// case-insensitively.

type itemKind int

const (
	kindInt itemKind = iota
	kindString
	kindList
)

type item struct {
	kind  itemKind
	num   string // kindInt: decimal digits without leading zeros ("" means 0)
	str   string // kindString: comparable qualifier
	items []*item
}

var qualifierOrder = []string{"alpha", "beta", "milestone", "rc", "snapshot", "", "sp"}

// releaseQualifier is the comparable form of the empty qualifier.
var releaseQualifier = comparableQualifier("")

func comparableQualifier(q string) string {
	for i, k := range qualifierOrder {
		if k == q {
			return strconv.Itoa(i)
		}
	}
	return strconv.Itoa(len(qualifierOrder)) + "-" + q
}

func newIntItem(s string) *item {
	return &item{kind: kindInt, num: strings.TrimLeft(s, "0")}
}

func newStringItem(s string, followedByDigit bool) *item {
	if followedByDigit && len(s) == 1 {
		switch s {
		case "a":
			s = "alpha"
		case "b":
			s = "beta"
		case "m":
			s = "milestone"
		}
	}
	switch s {
	case "ga", "final", "release":
		s = ""
	case "cr":
		s = "rc"
	}
	return &item{kind: kindString, str: comparableQualifier(s)}
}

func (it *item) isNull() bool {
	switch it.kind {
	case kindInt:
		return it.num == ""
	case kindString:
		return it.str == releaseQualifier
	default:
		return len(it.items) == 0
	}
}

func (it *item) normalize() {
	for i := len(it.items) - 1; i >= 0; i-- {
		last := it.items[i]
		if last.isNull() {
			it.items = append(it.items[:i], it.items[i+1:]...)
		} else if last.kind != kindList {
			break
		}
	}
}

func compareNum(a, b string) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// compare returns <0, 0, >0 comparing it with o; o may be nil (the "null" item).
func (it *item) compare(o *item) int {
	switch it.kind {
	case kindInt:
		if o == nil {
			if it.num == "" {
				return 0
			}
			return 1
		}
		if o.kind == kindInt {
			return compareNum(it.num, o.num)
		}
		return 1 // 1.1 > 1-sp, 1.1 > 1-1
	case kindString:
		if o == nil {
			return strings.Compare(it.str, releaseQualifier)
		}
		if o.kind == kindString {
			return strings.Compare(it.str, o.str)
		}
		return -1 // 1.any < 1.1, 1.any < 1-1
	default:
		if o == nil {
			if len(it.items) == 0 {
				return 0
			}
			return it.items[0].compare(nil)
		}
		switch o.kind {
		case kindInt:
			return -1 // 1-1 < 1.0.x
		case kindString:
			return 1 // 1-1 > 1-sp
		}
		n := max(len(it.items), len(o.items))
		for i := range n {
			var l, r *item
			if i < len(it.items) {
				l = it.items[i]
			}
			if i < len(o.items) {
				r = o.items[i]
			}
			var c int
			switch {
			case l == nil && r == nil:
				c = 0
			case l == nil:
				c = -r.compare(nil)
			default:
				c = l.compare(r)
			}
			if c != 0 {
				return c
			}
		}
		return 0
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func parseItem(isNum bool, s string) *item {
	if isNum {
		return newIntItem(s)
	}
	return newStringItem(s, false)
}

func parseVersion(v string) *item {
	v = strings.ToLower(v)
	root := &item{kind: kindList}
	list := root
	stack := []*item{root}
	digit := false
	start := 0
	push := func() {
		l := &item{kind: kindList}
		list.items = append(list.items, l)
		list = l
		stack = append(stack, l)
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '.' || c == '-':
			if i == start {
				list.items = append(list.items, newIntItem("0"))
			} else {
				list.items = append(list.items, parseItem(digit, v[start:i]))
			}
			start = i + 1
			if c == '-' {
				push()
			}
		case isDigit(c):
			if !digit && i > start {
				list.items = append(list.items, newStringItem(v[start:i], true))
				start = i
				push()
			}
			digit = true
		default:
			if digit && i > start {
				list.items = append(list.items, parseItem(true, v[start:i]))
				start = i
				push()
			}
			digit = false
		}
	}
	if len(v) > start {
		list.items = append(list.items, parseItem(digit, v[start:]))
	}
	for i := len(stack) - 1; i >= 0; i-- {
		stack[i].normalize()
	}
	return root
}

// CompareVersions orders two Maven versions: <0 if a < b, 0 if equal, >0 if a > b.
func CompareVersions(a, b string) int {
	return parseVersion(a).compare(parseVersion(b))
}

// sortVersions sorts vs ascending in Maven order (stable for equal versions).
func sortVersions(vs []string) {
	parsed := make(map[string]*item, len(vs))
	for _, v := range vs {
		parsed[v] = parseVersion(v)
	}
	sort.SliceStable(vs, func(i, j int) bool { return parsed[vs[i]].compare(parsed[vs[j]]) < 0 })
}

// isSnapshot reports whether v is a SNAPSHOT base version ("1.0-SNAPSHOT").
func isSnapshot(v string) bool { return strings.HasSuffix(v, snapshotSuffix) }

const snapshotSuffix = "-SNAPSHOT"
