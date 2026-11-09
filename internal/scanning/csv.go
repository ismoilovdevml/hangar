package scanning

import (
	"encoding/csv"
	"io"
	"slices"
	"strconv"
	"strings"
)

// WriteFixPlanCSV writes the packages of p as CSV, one row per package, in
// the order of the plan. Cells that a spreadsheet would read as a formula
// are prefixed with an apostrophe.
func WriteFixPlanCSV(w io.Writer, p *FixPlan) error {
	cw := csv.NewWriter(w)
	window := strconv.Itoa(p.WindowDays) + "d"
	header := []string{"package", "format", "repositories", "advisories", "vulnerable_versions", "findings", "max_severity",
		"max_cvss", "upgrade_to", "upgrade_commands", "pulls_" + window, "clients_" + window, "in_use", "assignee", "status"}
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, pkg := range p.Items {
		ids := make([]string, 0, len(pkg.Advisories))
		for _, a := range pkg.Advisories {
			id := a.ID
			for _, alias := range a.Aliases {
				if strings.HasPrefix(alias, "CVE-") {
					id += " (" + alias + ")"
					break
				}
			}
			ids = append(ids, id)
		}
		var targets, commands []string
		if pkg.Image != nil {
			// Images are rebuilt; the command upgrades their OS packages.
			targets = append(targets, "rebuild ("+strconv.Itoa(pkg.Fixable)+" of "+strconv.Itoa(pkg.Findings)+" findings have a fixed package)")
			if pkg.Image.Command != nil {
				commands = append(commands, *pkg.Image.Command)
			}
		}
		for _, u := range pkg.Upgrades {
			if pkg.Image != nil {
				break
			}
			if u.To == nil {
				targets = append(targets, "no fix for "+strings.Join(u.From, ", "))
				continue
			}
			targets = append(targets, *u.To)
			if u.Command != nil {
				commands = append(commands, *u.Command)
			}
		}
		cvss := ""
		if pkg.MaxCVSS != nil {
			cvss = strconv.FormatFloat(*pkg.MaxCVSS, 'f', 1, 64)
		}
		assignee := ""
		switch {
		case pkg.Assignee != nil:
			assignee = *pkg.Assignee
		case pkg.AssigneeMixed:
			assignee = "several"
		}
		status := "open"
		if p.Tab == TabWaived {
			status = "waived"
		}
		row := []string{pkg.Package, pkg.Format, strings.Join(pkg.Repositories, " "), strings.Join(ids, "; "),
			strings.Join(pkg.Versions, " "), strconv.Itoa(pkg.Findings), pkg.MaxSeverity, cvss,
			strings.Join(dedupeStrings(targets), " / "), strings.Join(dedupeStrings(commands), "\n"),
			strconv.FormatInt(pkg.Pulls, 10), strconv.Itoa(pkg.Clients), strconv.FormatBool(pkg.InUse), assignee, status}
		for i, c := range row {
			row[i] = csvSafe(c)
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// csvSafe keeps spreadsheets from evaluating a cell as a formula.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func dedupeStrings(list []string) []string {
	var out []string
	for _, s := range list {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}
