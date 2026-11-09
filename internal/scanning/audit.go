package scanning

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ComponentKey names one package version: a component's namespace, name
// and version as stored.
type ComponentKey struct {
	Namespace, Name, Version string
}

// VersionReport is what the scanner knows about one stored package
// version, for clients' own audit commands (npm audit).
type VersionReport struct {
	ComponentKey
	RepositoryID string
	ComponentID  string
	// ScanStatus is the last scan's outcome: "ok", "failed",
	// "unsupported", or "" when the component was never scanned.
	ScanStatus string
	// Findings are the component's findings that are not fixed (open,
	// assigned or waived), without their long details.
	Findings []*Finding
}

// maxReportKeys bounds one VersionFindings call.
const maxReportKeys = 20000

// VersionFindings reports, for every key stored as a component in one of
// repoIDs, its scan status and findings. Keys that are not stored are left
// out. It reads stored results only; nothing is scanned.
func (s *Service) VersionFindings(ctx context.Context, repoIDs []string, keys []ComponentKey) ([]*VersionReport, error) {
	if len(repoIDs) == 0 || len(keys) == 0 {
		return nil, nil
	}
	if len(keys) > maxReportKeys {
		keys = keys[:maxReportKeys]
	}
	ns, names, versions := make([]string, len(keys)), make([]string, len(keys)), make([]string, len(keys))
	for i, k := range keys {
		ns[i], names[i], versions[i] = k.Namespace, k.Name, k.Version
	}
	rows, err := s.store.pool.Query(ctx, `
SELECT c.id::text, c.repo_id::text, c.namespace, c.name, c.version, coalesce(cs.status, '')
FROM (SELECT DISTINCT * FROM unnest($1::text[], $2::text[], $3::text[]) AS k(ns, name, version)) k
JOIN components c ON c.namespace = k.ns AND c.name = k.name AND c.version = k.version
LEFT JOIN component_scans cs ON cs.component_id = c.id
WHERE c.repo_id = ANY($4::uuid[])`, ns, names, versions, repoIDs)
	if err != nil {
		return nil, fmt.Errorf("version findings: %w", err)
	}
	reports, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*VersionReport, error) {
		var v VersionReport
		err := r.Scan(&v.ComponentID, &v.RepositoryID, &v.Namespace, &v.Name, &v.Version, &v.ScanStatus)
		return &v, err
	})
	if err != nil {
		return nil, fmt.Errorf("version findings: %w", err)
	}
	if len(reports) == 0 {
		return reports, nil
	}
	byID := make(map[string]*VersionReport, len(reports))
	ids := make([]string, 0, len(reports))
	for _, v := range reports {
		byID[v.ComponentID] = v
		ids = append(ids, v.ComponentID)
	}
	rows, err = s.store.pool.Query(ctx, fmt.Sprintf("SELECT "+findingCols, "''", "0")+findingFrom+
		` WHERE f.component_id = ANY($1::uuid[]) AND f.status <> 'fixed' AND f.kind = 'vulnerability'`, ids)
	if err != nil {
		return nil, fmt.Errorf("version findings: %w", err)
	}
	findings, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Finding, error) { return scanFinding(r) })
	if err != nil {
		return nil, fmt.Errorf("version findings: %w", err)
	}
	for _, f := range findings {
		if v := byID[f.Component.ID]; v != nil {
			v.Findings = append(v.Findings, f)
		}
	}
	return reports, nil
}
