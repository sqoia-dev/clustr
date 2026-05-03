package alerts

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sqoia-dev/clustr/internal/db"
)

// fakeStats implements StatsQuerier for testing.
type fakeStats struct {
	rows map[string][]db.NodeStatRow // key = nodeID+plugin+sensor
}

func (f *fakeStats) QueryNodeStats(ctx context.Context, p db.QueryNodeStatsParams) ([]db.NodeStatRow, bool, error) {
	key := p.NodeID + "|" + p.Plugin + "|" + p.Sensor
	rows, ok := f.rows[key]
	if !ok {
		return nil, false, nil
	}
	// Filter by time window.
	var out []db.NodeStatRow
	for _, r := range rows {
		if !r.TS.Before(p.Since) && !r.TS.After(p.Until) {
			out = append(out, r)
		}
	}
	return out, false, nil
}

func TestLoadRuleFile(t *testing.T) {
	dir := t.TempDir()
	content := `
name: disk-percent
description: Disk usage above threshold
plugin: disks
sensor: used_pct
labels:
  mount: ".*"
threshold:
  op: ">="
  value: 90
duration: 300s
severity: warn
notify:
  webhook: true
  email: ["ops@example.com"]
`
	path := filepath.Join(dir, "disk-percent.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	rules, err := loadRuleFile(path)
	if err != nil {
		t.Fatalf("loadRuleFile: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	r := rules[0]
	if r.Name != "disk-percent" {
		t.Errorf("Name = %q, want %q", r.Name, "disk-percent")
	}
	if r.Plugin != "disks" {
		t.Errorf("Plugin = %q, want %q", r.Plugin, "disks")
	}
	if r.Threshold.Op != ">=" {
		t.Errorf("Op = %q, want %q", r.Threshold.Op, ">=")
	}
	if r.Threshold.Value != 90 {
		t.Errorf("Value = %v, want 90", r.Threshold.Value)
	}
	if r.Severity != "warn" {
		t.Errorf("Severity = %q, want %q", r.Severity, "warn")
	}
	if r.Duration != 300*time.Second {
		t.Errorf("Duration = %v, want 300s", r.Duration)
	}
	if !r.Notify.Webhook {
		t.Error("Notify.Webhook should be true")
	}
}

func TestLoadMalformedRuleFile(t *testing.T) {
	dir := t.TempDir()
	// Missing required fields.
	content := `
name: ""
plugin: ""
sensor: ""
threshold:
  op: "bad"
  value: 0
severity: "unknown"
`
	path := filepath.Join(dir, "bad.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadRuleFile(path)
	if err == nil {
		t.Fatal("expected error for malformed rule, got nil")
	}
}

func TestGroupByLabels(t *testing.T) {
	now := time.Now()
	rows := []db.NodeStatRow{
		{NodeID: "n1", Plugin: "disks", Sensor: "used_pct", Value: 91, Labels: map[string]string{"mount": "/var"}, TS: now},
		{NodeID: "n1", Plugin: "disks", Sensor: "used_pct", Value: 92, Labels: map[string]string{"mount": "/var"}, TS: now.Add(time.Second)},
		{NodeID: "n1", Plugin: "disks", Sensor: "used_pct", Value: 50, Labels: map[string]string{"mount": "/boot"}, TS: now},
	}
	groups := groupByLabels(rows)
	// Should be 2 groups: {"mount":"/var"} and {"mount":"/boot"}.
	if len(groups) != 2 {
		t.Errorf("expected 2 groups, got %d", len(groups))
	}
}

func TestLabelsToJSON(t *testing.T) {
	// nil map → ""
	if got := labelsToJSON(nil); got != "" {
		t.Errorf("labelsToJSON(nil) = %q, want %q", got, "")
	}
	// non-empty → JSON
	j := labelsToJSON(map[string]string{"mount": "/var"})
	if j == "" {
		t.Error("labelsToJSON with data should not return empty string")
	}
}

func TestEngineReloadRules(t *testing.T) {
	dir := t.TempDir()

	ruleContent := `
name: test-rule
plugin: disks
sensor: used_pct
threshold:
  op: ">="
  value: 90
severity: warn
notify:
  webhook: false
`
	path := filepath.Join(dir, "test-rule.yml")
	if err := os.WriteFile(path, []byte(ruleContent), 0o644); err != nil {
		t.Fatal(err)
	}

	engine := &Engine{
		rulesDir:   dir,
		stats:      &fakeStats{},
		ruleMtimes: make(map[string]time.Time),
	}
	engine.reloadRulesIfChanged()

	rules := engine.Rules()
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].Name != "test-rule" {
		t.Errorf("rule name = %q, want %q", rules[0].Name, "test-rule")
	}

	// A second call with no changes should not alter the rule set.
	engine.reloadRulesIfChanged()
	rules2 := engine.Rules()
	if len(rules2) != 1 {
		t.Errorf("expected 1 rule after no-change reload, got %d", len(rules2))
	}
}
