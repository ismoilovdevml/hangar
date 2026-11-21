package cac

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
)

// Apply carries out the plan and reports each step on w. Changes run in
// plan order. When one fails, the changes of later stages are skipped, as
// they may depend on it (a repository on its blob store), and so is every
// delete; the other changes of the same stage still run. The error says how
// many changes failed.
func Apply(ctx context.Context, d Doer, p *Plan, w io.Writer) error {
	failedStage := -1
	var failed, skipped, done int
	for _, c := range p.Changes {
		if c.Action == ActionUnchanged {
			continue
		}
		if failedStage >= 0 && (c.Action == ActionDelete || stageOf(c) > failedStage) {
			fmt.Fprintf(w, "  %s skipped: an earlier change failed\n", c.ID())
			skipped++
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		warnings, err := applyChange(ctx, d, c)
		for _, wn := range warnings {
			fmt.Fprintf(w, "    warning: %s\n", wn)
		}
		if err != nil {
			fmt.Fprintf(w, "! %s %s failed: %s\n", c.ID(), c.Action, errText(err))
			failed++
			switch {
			case c.Action == ActionDelete:
				failedStage = 0 // deletes run last: this skips the remaining ones
			case failedStage < 0 || stageOf(c) < failedStage:
				failedStage = stageOf(c)
			}
			continue
		}
		fmt.Fprintf(w, "%s %s\n", c.ID(), map[Action]string{ActionCreate: "created", ActionUpdate: "updated", ActionDelete: "deleted"}[c.Action])
		done++
	}
	fmt.Fprintf(w, "\nApplied %d %s", done, plural(done, "change"))
	if failed+skipped > 0 {
		fmt.Fprintf(w, ", %d failed, %d skipped", failed, skipped)
	}
	fmt.Fprintln(w, ".")
	if failed > 0 {
		return fmt.Errorf("%d of %d changes failed", failed, failed+skipped+done)
	}
	return nil
}

func applyChange(ctx context.Context, d Doer, c *Change) ([]string, error) {
	switch c.Action {
	case ActionCreate:
		body, err := withPreview(ctx, d, c)
		if err != nil {
			return nil, err
		}
		var answer map[string]any
		if err := d.Do(ctx, http.MethodPost, c.k.Collection, body, &answer); err != nil {
			return nil, err
		}
		warnings := notKept(c, answer)
		if err := putLabels(ctx, d, c); err != nil {
			return warnings, fmt.Errorf("created, but labelling it failed (the next apply adopts it): %w", err)
		}
		return warnings, nil
	case ActionUpdate:
		var warnings []string
		if len(c.Diffs) > 0 || c.ResendSecrets {
			body, err := withPreview(ctx, d, c)
			if err != nil {
				return nil, err
			}
			var answer map[string]any
			if err := d.Do(ctx, http.MethodPut, c.k.itemPath(c.Name), body, &answer); err != nil {
				return nil, err
			}
			warnings = notKept(c, answer)
		}
		if c.SetLabels != nil {
			if err := putLabels(ctx, d, c); err != nil {
				return warnings, err
			}
		}
		return warnings, nil
	case ActionDelete:
		err := d.Do(ctx, http.MethodDelete, c.k.itemPath(c.Name), nil, nil)
		if isStatus(err, http.StatusNotFound) {
			return []string{"already gone"}, nil
		}
		return nil, err
	}
	return nil, nil
}

// withPreview returns the request body; cleanup policies get a fresh
// preview_token, which the server requires to save selection rules.
func withPreview(ctx context.Context, d Doer, c *Change) (map[string]any, error) {
	if c.k.Name != KindCleanupPolicy {
		return c.body, nil
	}
	var prev struct {
		PreviewToken string `json:"preview_token"`
	}
	if err := d.Do(ctx, http.MethodPost, "/cleanup-policies/preview?sample=1", c.body, &prev); err != nil {
		return nil, fmt.Errorf("preview: %w", err)
	}
	if prev.PreviewToken == "" {
		return nil, errors.New("preview: the server issued no preview_token")
	}
	body := maps.Clone(c.body)
	body["preview_token"] = prev.PreviewToken
	return body, nil
}

func putLabels(ctx context.Context, d Doer, c *Change) error {
	return d.Do(ctx, http.MethodPut, "/resource-labels/"+c.k.LabelKind+"/"+escapePath(c.Name),
		map[string]any{"labels": c.SetLabels}, nil)
}

// notKept compares the server's answer with the declaration: a field the
// server did not keep is unknown to it, misspelled or normalized in a way
// the comparison does not know, and would show up as a change on every run.
func notKept(c *Change, answer map[string]any) []string {
	if c.Doc == nil || answer == nil {
		return nil
	}
	answerJSON, err := toJSON(answer)
	if err != nil {
		return nil
	}
	m, _ := answerJSON.(map[string]any)
	var out []string
	for _, f := range c.k.diffSpec(c.Doc.Spec, c.k.project(m)) {
		out = append(out, fmt.Sprintf("spec.%s: the server answered %s, not %s (unknown or misspelled field?)",
			f.Path, renderValue(c, f.Path, f.Old, f.OldSet), renderValue(c, f.Path, f.New, f.New != nil)))
	}
	return out
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
