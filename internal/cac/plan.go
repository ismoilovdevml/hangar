package cac

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/api/labels"
)

// Options control a plan.
type Options struct {
	// ApplySet names the configuration that owns the resources: every
	// resource apply creates or adopts is labelled with it, and prune only
	// considers resources carrying it.
	ApplySet string
	// Prune plans deleting the resources of ApplySet that the files no
	// longer declare.
	Prune bool
	// TakeOwnership lets the files adopt resources another apply set owns.
	TakeOwnership bool
	// UpdateSecrets sends declared secrets again. The API never returns
	// secrets, so a changed secret value is otherwise not noticed.
	UpdateSecrets bool
}

// Action is what apply does to one resource.
type Action string

// Actions.
const (
	ActionCreate    Action = "create"
	ActionUpdate    Action = "update"
	ActionDelete    Action = "delete"
	ActionUnchanged Action = "unchanged"
)

// Change is the planned action on one resource.
type Change struct {
	Action Action
	Kind   string
	Name   string
	// Doc is the declaration (nil for a delete).
	Doc *Document
	// Diffs are the spec fields an update changes.
	Diffs []FieldDiff
	// SetLabels are the labels written after the create or update; nil
	// when the resource's labels stay as they are. OldLabels are the
	// labels it has now.
	SetLabels map[string]string
	OldLabels map[string]string
	// ResendSecrets: the update only sends secrets again (UpdateSecrets).
	ResendSecrets bool
	// Notes are remarks shown with the change.
	Notes []string

	k     *kind
	body  map[string]any // request body of a create or update
	group []string       // members, when the resource is a group repository
}

// ID is "Kind/name".
func (c *Change) ID() string { return c.Kind + "/" + c.Name }

// Plan is the list of changes in the order apply carries them out: creates
// and updates by dependency (stage), then deletes in reverse.
type Plan struct {
	Server   string
	ApplySet string
	Changes  []*Change
	// Warnings concern the plan as a whole.
	Warnings []string
}

// Counts returns the number of changes per action.
func (p *Plan) Counts() map[Action]int {
	n := map[Action]int{}
	for _, c := range p.Changes {
		n[c.Action]++
	}
	return n
}

// HasChanges reports whether applying the plan would change anything.
func (p *Plan) HasChanges() bool {
	n := p.Counts()
	return n[ActionCreate]+n[ActionUpdate]+n[ActionDelete] > 0
}

// liveSet is the state of one kind on the server.
type liveSet struct {
	objects map[string]map[string]any    // GET answers by name
	labels  map[string]map[string]string // labels by name
}

// MakePlan compares docs with the server and returns what apply would do.
func MakePlan(ctx context.Context, d Doer, server string, docs []*Document, o Options) (*Plan, error) {
	if err := checkApplySet(o.ApplySet); err != nil {
		return nil, err
	}
	p := &Plan{Server: server, ApplySet: o.ApplySet}
	declared := map[string]bool{}
	for _, doc := range docs {
		declared[doc.ID()] = true
	}
	live := map[string]*liveSet{}
	for _, k := range kinds {
		inFiles := slices.ContainsFunc(docs, func(doc *Document) bool { return doc.Kind == k.Name })
		if !inFiles && !o.Prune {
			continue
		}
		ls, err := fetchLive(ctx, d, k)
		if err != nil {
			if !inFiles && (isStatus(err, http.StatusForbidden) || isStatus(err, http.StatusUnauthorized)) {
				p.Warnings = append(p.Warnings, fmt.Sprintf("%s resources are not pruned: the token may not read them (%v)", k.Name, err))
				continue
			}
			return nil, err
		}
		live[k.Name] = ls
	}

	var errs []error
	for _, doc := range docs {
		c, err := planDocument(ctx, d, doc, live[doc.Kind], o)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s (%s): %w", doc.ID(), doc.Source, err))
			continue
		}
		p.Changes = append(p.Changes, c)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if o.Prune {
		for _, k := range kinds {
			ls := live[k.Name]
			if ls == nil {
				continue
			}
			for _, name := range slices.Sorted(maps.Keys(ls.labels)) {
				l := ls.labels[name]
				obj, exists := ls.objects[name]
				if declared[k.Name+"/"+name] || !exists || !ownedBy(l, o.ApplySet) || protected(k, obj) != "" {
					continue
				}
				c := &Change{Action: ActionDelete, Kind: k.Name, Name: name, k: k, OldLabels: l,
					Notes: []string{"no longer declared"}}
				if k.Name == KindRepository {
					c.Notes = append(c.Notes, "deleting a repository deletes its content")
				}
				if members, ok := getPath(obj, Path{"config", "group", "members"}); ok {
					c.group = stringList(members)
				}
				p.Changes = append(p.Changes, c)
			}
		}
	}
	ordered, err := order(p.Changes)
	if err != nil {
		return nil, err
	}
	p.Changes = ordered
	return p, nil
}

func checkApplySet(set string) error {
	if set == "" {
		return errors.New("an apply set name is required")
	}
	if err := labels.Validate(map[string]string{LabelApplySet: set}); err != nil || strings.ContainsAny(set, " /") {
		return fmt.Errorf("apply set %q: use letters, digits, '-', '_' and '.'", set)
	}
	return nil
}

// ownedBy reports whether labels mark a resource as created or adopted by
// apply set set.
func ownedBy(l map[string]string, set string) bool {
	return l[LabelManagedBy] == ManagedByApply && l[LabelApplySet] == set
}

// protected says why the server does not let apply manage a live object,
// or "" when it does.
func protected(k *kind, obj map[string]any) string {
	switch {
	case k.Name == KindRole && obj["builtin"] == true:
		return "built-in roles cannot be changed"
	case k.Name == KindOutboundProxy && obj["source"] == "file":
		return "it is defined in the server configuration file (http.proxy); change it there"
	case k.Name == KindBlobStore && obj["from_config"] == true:
		return "it is defined in the server configuration (storage.*); only its soft quota can change"
	}
	return ""
}

func planDocument(ctx context.Context, d Doer, doc *Document, ls *liveSet, o Options) (*Change, error) {
	k := kindByName(doc.Kind)
	c := &Change{Kind: doc.Kind, Name: doc.Name, Doc: doc, k: k}
	want := maps.Clone(doc.Labels)
	want[LabelManagedBy], want[LabelApplySet] = ManagedByApply, o.ApplySet
	obj, exists := ls.objects[doc.Name]
	if !exists {
		c.Action = ActionCreate
		c.body = withName(maps.Clone(doc.Spec), doc.Name)
		c.SetLabels = want
	} else {
		if why := protected(k, obj); why != "" && k.Name != KindBlobStore {
			return nil, errors.New(why)
		}
		have := ls.labels[doc.Name]
		c.OldLabels = have
		if have[LabelManagedBy] == ManagedByApply && have[LabelApplySet] != o.ApplySet {
			if !o.TakeOwnership {
				return nil, fmt.Errorf("owned by apply set %q, not %q; remove it from one of them, or pass --take-ownership to move it",
					have[LabelApplySet], o.ApplySet)
			}
		}
		liveSpec := k.project(obj)
		c.Diffs = append(k.diffSpec(doc.Spec, liveSpec), extraDiffs(k, doc, obj)...)
		c.body = withName(k.merge(nil, liveSpec, doc.Spec).(map[string]any), doc.Name)
		if !maps.Equal(have, want) {
			c.SetLabels = want
		}
		switch {
		case len(c.Diffs) > 0:
			c.Action = ActionUpdate
		case o.UpdateSecrets && hasSecrets(k, doc):
			c.Action, c.ResendSecrets = ActionUpdate, true
		case c.SetLabels != nil:
			c.Action = ActionUpdate
		default:
			c.Action = ActionUnchanged
		}
	}
	if members, ok := getPath(c.body, Path{"config", "group", "members"}); ok {
		c.group = stringList(members)
	}
	for _, p := range literalSecrets(k, doc) {
		c.Notes = append(c.Notes, fmt.Sprintf("spec.%s is written in the file; prefer valueFrom: {env: NAME} or {file: PATH}", p))
	}
	if k.Name == KindCleanupPolicy && (c.Action == ActionCreate || touches(c.Diffs, "format", "criteria", "repositories")) {
		c.Notes = append(c.Notes, previewNote(ctx, d, c.body))
	}
	return c, nil
}

func withName(m map[string]any, name string) map[string]any {
	m["name"] = name
	return m
}

func touches(diffs []FieldDiff, fields ...string) bool {
	return slices.ContainsFunc(diffs, func(f FieldDiff) bool {
		return len(f.Path) > 0 && slices.Contains(fields, f.Path[0])
	})
}

// extraDiffs are differences the generic comparison cannot see: whether an
// outbound proxy has a password is known although the password is not.
func extraDiffs(k *kind, doc *Document, obj map[string]any) []FieldDiff {
	if k.Name != KindOutboundProxy {
		return nil
	}
	configured := obj["password_configured"] == true
	pw, _ := doc.Spec["password"].(string)
	clearPw, _ := doc.Spec["clear_password"].(bool)
	switch {
	case pw != "" && !configured:
		return []FieldDiff{{Path: Path{"password"}, New: secretText}}
	case clearPw && configured:
		return []FieldDiff{{Path: Path{"password"}, Old: secretText, OldSet: true, New: nil}}
	}
	return nil
}

// hasSecrets reports whether the declaration carries a secret the server
// would store: a reference, a write-only field or a password in a URL.
func hasSecrets(k *kind, doc *Document) bool {
	if len(doc.Secrets) > 0 {
		return true
	}
	for _, p := range k.writeOnly {
		if v, ok := getPath(doc.Spec, p); ok && !isZero(v) && v != true {
			return true
		}
	}
	for _, p := range k.urlSecrets {
		if v, ok := getPath(doc.Spec, p); ok {
			if s, _ := v.(string); urlHasPassword(s) {
				return true
			}
		}
	}
	return false
}

// literalSecrets lists write-only fields written in the file instead of
// referenced.
func literalSecrets(k *kind, doc *Document) []Path {
	var out []Path
	for _, p := range k.writeOnly {
		v, ok := getPath(doc.Spec, p)
		if s, _ := v.(string); ok && s != "" && !slices.ContainsFunc(doc.Secrets, p.equal) {
			out = append(out, p)
		}
	}
	return out
}

func urlHasPassword(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return false
	}
	_, ok := u.User.Password()
	return ok
}

// previewNote previews a cleanup policy and says what it would delete now.
func previewNote(ctx context.Context, d Doer, body map[string]any) string {
	var prev struct {
		ComponentCount int64 `json:"component_count"`
		Bytes          int64 `json:"bytes"`
	}
	if err := d.Do(ctx, http.MethodPost, "/cleanup-policies/preview?sample=1", body, &prev); err != nil {
		return "preview not available: " + errText(err)
	}
	return fmt.Sprintf("would delete %d versions (%s) if it ran now", prev.ComponentCount, humanBytes(prev.Bytes))
}

func fetchLive(ctx context.Context, d Doer, k *kind) (*liveSet, error) {
	ls := &liveSet{objects: map[string]map[string]any{}}
	cursor := ""
	for {
		path := k.Collection
		if k.paged {
			path += "?limit=1000"
			if cursor != "" {
				path += "&cursor=" + url.QueryEscape(cursor)
			}
		}
		var page struct {
			Items      []map[string]any `json:"items"`
			NextCursor *string          `json:"next_cursor"`
			Limited    bool             `json:"limited"`
		}
		if err := d.Do(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		if page.Limited {
			return nil, fmt.Errorf("%s: the token may only see proxy names; it needs system:settings:read", k.Name)
		}
		for _, raw := range page.Items {
			obj, err := toJSON(raw)
			if err != nil {
				return nil, err
			}
			m := obj.(map[string]any)
			if name, _ := m["name"].(string); name != "" {
				ls.objects[name] = m
			}
		}
		if !k.paged || page.NextCursor == nil || *page.NextCursor == "" || len(page.Items) == 0 {
			break
		}
		cursor = *page.NextCursor
	}
	var lp struct {
		Items []struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"items"`
	}
	if err := d.Do(ctx, http.MethodGet, "/resource-labels?kind="+k.LabelKind, nil, &lp); err != nil {
		if isStatus(err, http.StatusNotFound) || isStatus(err, http.StatusNotImplemented) {
			return nil, fmt.Errorf("the server does not keep resource labels (Hangar 0.2.0 or later is needed): %w", err)
		}
		return nil, err
	}
	ls.labels = map[string]map[string]string{}
	for _, it := range lp.Items {
		ls.labels[it.Name] = it.Labels
	}
	return ls, nil
}

func stringList(v any) []string {
	xs, _ := v.([]any)
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// stageOf orders a change: groups come after the repositories they hold.
func stageOf(c *Change) int {
	s := c.k.stage
	if c.group != nil {
		s++
	}
	return s
}

// order puts creates and updates (and unchanged resources) in dependency
// order and deletes after them in reverse order. Groups inside groups are
// ordered so that a member group exists before the group holding it and is
// deleted after it.
func order(changes []*Change) ([]*Change, error) {
	var forward, deletes []*Change
	for _, c := range changes {
		if c.Action == ActionDelete {
			deletes = append(deletes, c)
		} else {
			forward = append(forward, c)
		}
	}
	byStage := func(a, b *Change) int {
		return cmp.Or(cmp.Compare(stageOf(a), stageOf(b)), cmp.Compare(a.Name, b.Name))
	}
	slices.SortFunc(forward, byStage)
	slices.SortFunc(deletes, byStage)
	forward, err := groupsInOrder(forward)
	if err != nil {
		return nil, err
	}
	deletes, err = groupsInOrder(deletes)
	if err != nil {
		return nil, err
	}
	slices.Reverse(deletes)
	return append(forward, deletes...), nil
}

// groupsInOrder reorders the group changes of a sorted list so that every
// group follows the groups among its members (Kahn's algorithm, by name).
func groupsInOrder(cs []*Change) ([]*Change, error) {
	var groups []*Change
	for _, c := range cs {
		if c.group != nil && c.Kind == KindRepository {
			groups = append(groups, c)
		}
	}
	if len(groups) < 2 {
		return cs, nil
	}
	index := map[string]*Change{}
	for _, g := range groups {
		index[g.Name] = g
	}
	placed := map[string]bool{}
	var sorted []*Change
	for len(sorted) < len(groups) {
		progress := false
		for _, g := range groups {
			if placed[g.Name] {
				continue
			}
			ready := true
			for _, m := range g.group {
				if index[m] != nil && !placed[m] && m != g.Name {
					ready = false
				}
			}
			if ready {
				placed[g.Name] = true
				sorted = append(sorted, g)
				progress = true
			}
		}
		if !progress {
			var cycle []string
			for _, g := range groups {
				if !placed[g.Name] {
					cycle = append(cycle, g.Name)
				}
			}
			return nil, fmt.Errorf("groups %s contain each other", strings.Join(cycle, ", "))
		}
	}
	// Groups take the place of the first group in the sorted list, which
	// keeps the stages in order.
	out := make([]*Change, 0, len(cs))
	inserted := false
	for _, c := range cs {
		if c.group != nil && c.Kind == KindRepository {
			if !inserted {
				out = append(out, sorted...)
				inserted = true
			}
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// Render writes the plan in a readable form; verbose also lists unchanged
// resources.
func (p *Plan) Render(w io.Writer, verbose bool) {
	fmt.Fprintf(w, "Hangar %s, apply set %q\n\n", p.Server, p.ApplySet)
	for _, warn := range p.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warn)
	}
	shown := 0
	for _, c := range p.Changes {
		if c.Action == ActionUnchanged && !verbose && len(c.Notes) == 0 {
			continue
		}
		shown++
		sign := map[Action]string{ActionCreate: "+", ActionUpdate: "~", ActionDelete: "-", ActionUnchanged: "="}[c.Action]
		fmt.Fprintf(w, "%s %s %s\n", sign, c.ID(), c.Action)
		for _, f := range c.Diffs {
			fmt.Fprintf(w, "    %s: %s -> %s\n", f.Path, renderValue(c, f.Path, f.Old, f.OldSet), renderValue(c, f.Path, f.New, f.New != nil))
		}
		if c.ResendSecrets {
			fmt.Fprintf(w, "    secrets: sent again (--update-secrets)\n")
		}
		if c.Action != ActionCreate {
			for _, line := range labelLines(c, p.ApplySet) {
				fmt.Fprintf(w, "    %s\n", line)
			}
		}
		for _, n := range c.Notes {
			fmt.Fprintf(w, "    note: %s\n", n)
		}
	}
	if shown > 0 {
		fmt.Fprintln(w)
	}
	n := p.Counts()
	fmt.Fprintf(w, "Plan: %d to create, %d to update, %d to delete, %d unchanged.\n",
		n[ActionCreate], n[ActionUpdate], n[ActionDelete], n[ActionUnchanged])
}

func renderValue(c *Change, p Path, v any, set bool) string {
	var secrets []Path
	if c.Doc != nil {
		secrets = c.Doc.Secrets
	}
	return render(c.k.mask(p, v, secrets), set)
}

// labelLines describes a label change.
func labelLines(c *Change, set string) []string {
	if c.SetLabels == nil {
		return nil
	}
	old := c.OldLabels
	var out []string
	switch {
	case old[LabelManagedBy] != ManagedByApply:
		out = append(out, fmt.Sprintf("labels: adopted into apply set %q", set))
	case old[LabelApplySet] != set:
		out = append(out, fmt.Sprintf("labels: moved from apply set %q to %q", old[LabelApplySet], set))
	}
	keys := slices.Sorted(maps.Keys(c.SetLabels))
	for k := range old {
		if _, ok := c.SetLabels[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		if strings.HasPrefix(k, LabelPrefix) {
			continue
		}
		o, had := old[k]
		n, has := c.SetLabels[k]
		switch {
		case had && !has:
			out = append(out, fmt.Sprintf("labels.%s: %q -> (unset)", k, o))
		case !had && has:
			out = append(out, fmt.Sprintf("labels.%s: (unset) -> %q", k, n))
		case o != n:
			out = append(out, fmt.Sprintf("labels.%s: %q -> %q", k, o, n))
		}
	}
	return out
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// errText describes an error for a person: an API error as its status,
// code and message, with the fields the server named.
func errText(err error) string {
	var ae *APIError
	if !errors.As(err, &ae) {
		return err.Error()
	}
	s := fmt.Sprintf("%d %s: %s", ae.Status, cmp.Or(ae.Code, "error"), cmp.Or(ae.Message, http.StatusText(ae.Status)))
	for _, f := range slices.Sorted(maps.Keys(ae.Fields)) {
		s += fmt.Sprintf("\n      %s: %s", f, ae.Fields[f])
	}
	return s
}
