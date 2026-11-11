package sysmon

import (
	"cmp"
	"fmt"
	"slices"
	"time"
)

// CheckState is one subsystem check of one node, or a node that stopped
// publishing heartbeats (Name "node").
type CheckState struct {
	// Node is the node the check ran on; set when there is more than one.
	Node   string `json:"node,omitempty"`
	Name   string `json:"name"`
	Label  string `json:"label"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Since is when the node first saw this status.
	Since     *time.Time `json:"since,omitempty"`
	LatencyMS *int64     `json:"latency_ms,omitempty"`
}

// Summary is the state of the whole installation. Every health indicator is
// built from it, so the status screen, the public status endpoint and the
// navigation agree.
type Summary struct {
	// Status is the worst status: the worst check of the nodes that answer,
	// at least degraded while a node is silent, down when none answers.
	Status string `json:"status"`
	// Headline says it in a few words: "All systems normal", "Blob storage
	// down", "2 checks need attention".
	Headline string `json:"headline"`
	Up       int    `json:"nodes_up"`
	Total    int    `json:"nodes_total"`
	// Checks and Passing count the subsystem checks of the nodes that answer.
	Checks  int `json:"checks_total"`
	Passing int `json:"checks_passing"`
	// Problems are the checks that are not healthy and the silent nodes,
	// worst first.
	Problems []CheckState `json:"problems"`
	// All are every check of the nodes that answer, in node order.
	All []CheckState `json:"-"`
}

// Summarize rolls nodes up into the state of the installation.
func Summarize(nodes []Node) Summary {
	s := Summary{Status: StatusHealthy, Total: len(nodes), Problems: []CheckState{}}
	multi := len(nodes) > 1
	for _, n := range nodes {
		if n.Stale {
			continue
		}
		s.Up++
		for _, sub := range n.Subsystems {
			c := CheckState{Name: sub.Name, Label: sub.Label, Status: cmp.Or(sub.Status, StatusHealthy), Detail: sub.Detail,
				Since: sub.Since, LatencyMS: sub.LatencyMS}
			if multi {
				c.Node = n.ID
			}
			s.Checks++
			s.All = append(s.All, c)
			if c.Status == StatusHealthy {
				s.Passing++
				continue
			}
			s.Status = worse(s.Status, c.Status)
			s.Problems = append(s.Problems, c)
		}
	}
	silent := s.Total - s.Up
	for _, n := range nodes {
		if n.Stale {
			s.Problems = append(s.Problems, CheckState{Node: n.ID, Name: "node", Label: "Node " + n.ID, Status: StatusDegraded,
				Detail: "No heartbeat since " + n.LastHeartbeat.UTC().Format(time.RFC3339)})
		}
	}
	switch {
	case s.Total > 0 && s.Up == 0:
		s.Status = StatusDown
	case silent > 0:
		s.Status = worse(s.Status, StatusDegraded)
	}
	slices.SortStableFunc(s.Problems, func(a, b CheckState) int { return rank(b.Status) - rank(a.Status) })
	s.Headline = headline(s, silent)
	return s
}

// headline words a summary; the web UI shows it as is.
func headline(s Summary, silent int) string {
	down, degraded := 0, 0
	for _, p := range s.Problems {
		if p.Name == "node" {
			continue
		}
		if p.Status == StatusDown {
			down++
		} else {
			degraded++
		}
	}
	switch {
	case s.Total == 0:
		return "Waiting for the first report"
	case s.Up == 0:
		return "No node is responding"
	case down+degraded == 1 && silent == 0:
		// "Blob storage down": no verb to agree with "Upstream proxies".
		p := s.Problems[0]
		if p.Status == StatusDown {
			return p.Label + " down"
		}
		return p.Label + " degraded"
	case down > 0:
		return count(down, "check is", "checks are") + " down"
	case degraded > 0:
		return count(degraded, "check needs", "checks need") + " attention"
	case silent > 0:
		return count(silent, "node is", "nodes are") + " not responding"
	}
	return "All systems normal"
}

func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func rank(s string) int {
	switch s {
	case StatusDown:
		return 2
	case StatusDegraded:
		return 1
	}
	return 0
}
