package main

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"
)

// messageBudget keeps each table message under Telegram's 4096-character
// limit, leaving room for the header and footer.
const messageBudget = 3500

// Table renders the metrics as MarkdownV2 messages: a header line and a
// monospace table, since Telegram does not render markdown tables. Rows are
// split across messages when the table would not fit in one.
func Table(label string, wallet *Series, groups []Grouped, hours int, end time.Time) []string {
	var table strings.Builder
	writer := tabwriter.NewWriter(&table, 0, 0, 1, ' ', tabwriter.AlignRight)
	fmt.Fprintln(writer, "strategy\tPnL\tMaxDD\tFees\tVol\tFills\tWin\tOpen\t")
	row := func(name string, s *Series) {
		m := s.Metrics
		win := "-"
		if m.Closed > 0 {
			win = fmt.Sprintf("%d/%d", m.Wins, m.Closed)
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t\n", name, usd(m.PnL), usd(m.MaxDrawdown),
			usd(m.Fees), usd(m.BuyVolume+m.SellVolume), m.Fills, win, usd(m.OpenCost))
	}
	row(WalletKey, wallet)
	for _, g := range groups {
		if len(g.Members) < 2 {
			row(g.Series.Key, g.Series)
			continue
		}
		row("["+g.Series.Key+"]", g.Series)
		for _, m := range g.Members {
			row("  "+m.Key, m)
		}
	}
	writer.Flush()
	lines := strings.SplitAfter(strings.TrimSuffix(table.String(), "\n"), "\n")
	header, rows := lines[0], lines[1:]

	var chunks [][]string
	for _, row := range rows {
		if n := len(chunks); n == 0 || size(chunks[n-1])+len(row) > messageBudget {
			chunks = append(chunks, nil)
		}
		chunks[len(chunks)-1] = append(chunks[len(chunks)-1], row)
	}

	code := strings.NewReplacer("\\", "\\\\", "`", "\\`")
	messages := make([]string, len(chunks))
	for i, chunk := range chunks {
		var out strings.Builder
		fmt.Fprintf(&out, "*PnL · %s*", escape(label))
		if len(chunks) > 1 {
			out.WriteString(escape(fmt.Sprintf(" (%d/%d)", i+1, len(chunks))))
		}
		fmt.Fprintf(&out, "\n%s\n", escape(fmt.Sprintf("last %dh to %s · all-time %s", hours, end.UTC().Format("2006-01-02 15:04 UTC"), usd(wallet.Metrics.AllTime))))
		out.WriteString("```\n")
		out.WriteString(code.Replace(header + strings.Join(chunk, "")))
		out.WriteString("\n```\n")
		if i == len(chunks)-1 {
			out.WriteString(escape("Realized PnL only: open positions are carried at cost (Open), not marked to market. Win = closed lanes that ended in profit."))
		}
		messages[i] = out.String()
	}
	return messages
}

func size(rows []string) int {
	n := 0
	for _, row := range rows {
		n += len(row)
	}
	return n
}

func escape(text string) string {
	var out strings.Builder
	for _, r := range text {
		if strings.ContainsRune("_*[]()~`>#+-=|{}.!\\", r) {
			out.WriteByte('\\')
		}
		out.WriteRune(r)
	}
	return out.String()
}
