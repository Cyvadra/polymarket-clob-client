package executiontest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Status string

const (
	StatusPass         Status = "PASS"
	StatusFail         Status = "FAIL"
	StatusSkip         Status = "SKIP"
	StatusInconclusive Status = "INCONCLUSIVE"
	StatusInfo         Status = "INFO"
)

type Report struct {
	Path      string
	RunID     string
	StartedAt time.Time
	mu        sync.Mutex
	file      *os.File
	results   []scenarioResult
	phases    []phaseResult
}

type scenarioResult struct {
	Name   string
	Status Status
	Detail string
}

type phaseResult struct {
	Name   string
	Status Status
	Shares string
	Price  string
	Detail string
}

func NewReport(dir string, cfg Config) (*Report, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create report directory: %w", err)
	}
	started := time.Now().UTC()
	runID := started.Format("20060102T150405.000000000Z")
	path := filepath.Join(dir, "executiontest-"+runID+".md")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create report: %w", err)
	}
	report := &Report{Path: path, RunID: runID, StartedAt: started, file: file}
	report.writeHeader(cfg)
	return report, nil
}

func (r *Report) writeHeader(cfg Config) {
	r.write(fmt.Sprintf("# Execution NATS Integration Test\n\n- Run ID: `%s`\n- Started: `%s`\n- NATS URL: `%s`\n- Condition ID: `%s`\n- Asset ID: `%s`\n- Outcome: `%s`\n- Target USD per BUY: `%s`\n- Buy limit: `%s`\n- Sell limit: `%s`\n\n> This report records NATS-visible evidence only. It does not prove signatures, database transactions, exchange fees, settlement, or that all account orders are absent.\n\n## Timeline\n\n", r.RunID, r.StartedAt.Format(time.RFC3339Nano), cfg.NATSURL, cfg.ConditionID, cfg.AssetID, cfg.Outcome, cfg.TargetUSD, cfg.BuyLimit, cfg.SellLimit))
}

func (r *Report) Command(subject, lane, action, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeLocked(fmt.Sprintf("- `%s` command `%s` lane `%s`: **%s** %s\n", time.Now().UTC().Format(time.RFC3339Nano), subject, lane, markdownCell(action), markdownCell(detail)))
}

// Scenario appends the result to the timeline as it happens, so a killed run
// still leaves evidence; the Scenarios table is rendered once on Close.
func (r *Report) Scenario(name string, status Status, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, scenarioResult{Name: name, Status: status, Detail: detail})
	r.writeLocked(fmt.Sprintf("- `%s` scenario `%s`: **%s** %s\n", time.Now().UTC().Format(time.RFC3339Nano), name, status, markdownCell(detail)))
}

// Phase appends a life-cycle phase result to the timeline as it happens; the
// Lifecycle table is rendered once on Close. shares and price are optional
// and shown as a dash when empty.
func (r *Report) Phase(name string, status Status, shares, price, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.phases = append(r.phases, phaseResult{Name: name, Status: status, Shares: shares, Price: price, Detail: detail})
	r.writeLocked(fmt.Sprintf("- `%s` phase `%s`: **%s** shares=%s price=%s %s\n", time.Now().UTC().Format(time.RFC3339Nano), name, status, orDash(shares), orDash(price), markdownCell(detail)))
}

func (r *Report) Evidence(message WireMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeLocked(fmt.Sprintf("\n#### NATS `%s` at `%s`\n\n```json\n%s\n```\n\n", message.Subject, message.ReceivedAt.Format(time.RFC3339Nano), strings.TrimSpace(string(message.Payload))))
}

func (r *Report) Note(title, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeLocked(fmt.Sprintf("\n#### %s\n\n%s\n\n", title, detail))
}

func (r *Report) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.phases) > 0 {
		r.writeLocked("\n## Lifecycle\n\n| Phase | Status | Shares | Price | Detail |\n|---|---|---|---|---|\n")
		phaseCounts := make(map[Status]int)
		for _, phase := range r.phases {
			phaseCounts[phase.Status]++
			r.writeLocked(fmt.Sprintf("| `%s` | **%s** | %s | %s | %s |\n", phase.Name, phase.Status, orDash(phase.Shares), orDash(phase.Price), markdownCell(phase.Detail)))
		}
		r.writeLocked(fmt.Sprintf("\nLifecycle — PASS: %d, FAIL: %d, SKIP: %d, INCONCLUSIVE: %d, INFO: %d\n", phaseCounts[StatusPass], phaseCounts[StatusFail], phaseCounts[StatusSkip], phaseCounts[StatusInconclusive], phaseCounts[StatusInfo]))
	}
	r.writeLocked("\n## Scenarios\n\n| Scenario | Status | Detail |\n|---|---|---|\n")
	for _, result := range r.results {
		r.writeLocked(fmt.Sprintf("| `%s` | **%s** | %s |\n", result.Name, result.Status, markdownCell(result.Detail)))
	}
	r.writeLocked("\n## Summary\n\n")
	counts := make(map[Status]int)
	for _, result := range r.results {
		counts[result.Status]++
	}
	r.writeLocked(fmt.Sprintf("PASS: %d, FAIL: %d, SKIP: %d, INCONCLUSIVE: %d\n", counts[StatusPass], counts[StatusFail], counts[StatusSkip], counts[StatusInconclusive]))
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

func (r *Report) write(value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeLocked(value)
}

func (r *Report) writeLocked(value string) {
	if r.file != nil {
		_, _ = r.file.WriteString(value)
		_ = r.file.Sync()
	}
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

func markdownCell(value string) string {
	return strings.NewReplacer("|", "\\|", "\n", " ", "\r", " ").Replace(value)
}
