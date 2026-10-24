package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"hmj-agent/internal/chat"
	"hmj-agent/internal/config"
	"hmj-agent/internal/terminal"
	"hmj-agent/internal/terminal/theme"
)

// HandleHealth executes a concurrent, token-frugal health check across all configured
// providers and models and prints a status table.
func HandleHealth(cfg config.Config, client *chat.Client) error {
	return HandleHealthTo(cfg, client, os.Stdout)
}

// HandleHealthTo executes the health check and writes the formatted output to out.
func HandleHealthTo(cfg config.Config, client *chat.Client, out io.Writer) error {
	models := cfg.AllModels()
	if len(models) == 0 {
		fmt.Fprintln(out, theme.Note("no models configured"))
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopHalt := terminal.StartHaltListener(cancel)
	defer stopHalt()

	total := len(models)
	spinner := terminal.NewSpinner(fmt.Sprintf("checking %d models...", total))
	spinner.Start()
	defer spinner.Stop()

	startTime := time.Now()
	results := client.CheckAllHealth(ctx, models, func(done, total int) {
		spinner.SetLabel(fmt.Sprintf("checking models [%d/%d]...", done, total))
	})
	spinner.Stop()

	if errors.Is(ctx.Err(), context.Canceled) {
		fmt.Fprintln(out, "\n"+theme.Caution("health check cancelled"))
		return nil
	}

	slices.SortStableFunc(results, compareHealthResults)

	rows, healthyCount, healthyProviders, providerCount := buildHealthRows(results)
	tbl := &terminal.Table{
		Headers: []string{"Provider", "Model", "Status", "Latency", "Details"},
		Align: []terminal.Alignment{
			terminal.AlignLeft,
			terminal.AlignLeft,
			terminal.AlignLeft,
			terminal.AlignRight,
			terminal.AlignLeft,
		},
		Rows: rows,
	}

	totalDuration := time.Since(startTime).Truncate(time.Millisecond)

	fmt.Fprintln(out)
	fmt.Fprintf(out, "%s\n", theme.Title(theme.ASCII("HMJ Agent — Provider & Model Health Check")))
	fmt.Fprint(out, tbl.Render())
	fmt.Fprintln(out)

	fmt.Fprintf(out, "%s %d/%d models healthy across %d/%d providers (took %s)\n",
		healthSummaryBadge(healthyCount, total),
		healthyCount, total,
		len(healthyProviders), providerCount,
		totalDuration,
	)
	fmt.Fprintf(out, "%s %s\n\n",
		theme.Hint("token efficiency:"),
		theme.Muted(fmt.Sprintf("~2 tokens per model (~%d tokens total, zero workspace guidelines sent)", 2*total)),
	)

	return nil
}

// compareHealthResults orders healthy models first (lowest latency first), then
// non-healthy models, grouping ties by provider and model.
func compareHealthResults(a, b chat.ModelHealthResult) int {
	aHealthy := (a.Status == chat.StatusHealthy)
	bHealthy := (b.Status == chat.StatusHealthy)
	if aHealthy != bHealthy {
		if aHealthy {
			return -1
		}
		return 1
	}
	// For healthy models, lowest latency first
	if aHealthy && bHealthy {
		if a.Latency != b.Latency {
			if a.Latency < b.Latency {
				return -1
			}
			return 1
		}
	}
	// For non-healthy models, group by provider and model
	if a.Ref.Provider != b.Ref.Provider {
		return strings.Compare(a.Ref.Provider, b.Ref.Provider)
	}
	return strings.Compare(a.Ref.Model, b.Ref.Model)
}

// buildHealthRows converts sorted results into table rows and returns the number of
// healthy models, the set of healthy provider names, and the count of all providers.
func buildHealthRows(results []chat.ModelHealthResult) (rows [][]string, healthyCount int, healthyProviders map[string]bool, providerCount int) {
	rows = make([][]string, len(results))
	providerSet := make(map[string]bool)
	healthyProviders = make(map[string]bool)

	for i, r := range results {
		providerSet[r.Ref.Provider] = true
		if r.Status == chat.StatusHealthy {
			healthyCount++
			healthyProviders[r.Ref.Provider] = true
		}

		modelLabel := r.Ref.Model
		if r.ModelName != "" && r.ModelName != r.Ref.Model {
			modelLabel = fmt.Sprintf("%s (%s)", r.Ref.Model, r.ModelName)
		}

		rows[i] = []string{
			r.Ref.Provider,
			modelLabel,
			healthStatusLabel(r.Status),
			healthLatencyLabel(r),
			healthDetailsLabel(r),
		}
	}
	return rows, healthyCount, healthyProviders, len(providerSet)
}

// healthStatusLabel returns the colored status cell for a health status.
func healthStatusLabel(status chat.HealthStatus) string {
	switch status {
	case chat.StatusHealthy:
		return theme.OK("OK")
	case chat.StatusAuthError:
		return theme.Danger("AUTH")
	case chat.StatusRateLimited:
		return theme.Caution("RATE")
	case chat.StatusTimeout:
		return theme.Caution("TIMEOUT")
	default:
		return theme.Danger("FAIL")
	}
}

// healthLatencyLabel returns the latency cell, or a dash when no latency applies.
func healthLatencyLabel(r chat.ModelHealthResult) string {
	if r.Status != chat.StatusTimeout && r.Latency > 0 {
		return fmt.Sprintf("%dms", r.Latency.Milliseconds())
	}
	return theme.ASCII("—")
}

// healthDetailsLabel returns the details cell, truncated to 60 runes for failures. A failure's
// details quote the provider's error body, so they are sanitized.
func healthDetailsLabel(r chat.ModelHealthResult) string {
	if r.Status == chat.StatusHealthy {
		return "healthy"
	}
	runes := []rune(strings.Join(strings.Fields(theme.Sanitize(r.Details)), " "))
	if len(runes) > 60 {
		return string(runes[:57]) + "..."
	}
	return string(runes)
}

// healthSummaryBadge picks the summary prefix colored by overall health.
func healthSummaryBadge(healthyCount, total int) string {
	if healthyCount < total {
		if healthyCount == 0 {
			return theme.Danger("health:")
		}
		return theme.Caution("health:")
	}
	return theme.OK("health:")
}
