package main

import (
	"fmt"
	"time"

	"github.com/go-analyze/charts"
)

const samples = 145

// Chart renders the series' window PnL as a PNG line chart.
func Chart(s *Series, title string, start, end time.Time) ([]byte, error) {
	times, values := s.Sample(start, end, samples)
	labels := make([]string, len(times))
	for i, t := range times {
		labels[i] = t.UTC().Format("01-02 15:04")
	}
	opt := charts.NewLineChartOptionWithData([][]float64{values})
	opt.Title.Text = title
	opt.Title.Subtext = fmt.Sprintf("realized PnL %s · max drawdown %s", usd(s.Metrics.PnL), usd(s.Metrics.MaxDrawdown))
	opt.XAxis.Labels = labels
	opt.XAxis.Unit = (samples - 1) / 6
	opt.XAxis.BoundaryGap = charts.Ptr(false)
	opt.YAxis = []charts.YAxisOption{{ValueFormatter: func(v float64) string { return usd(v) }}}
	opt.Symbol.Shape = charts.SymbolNone
	opt.LineStrokeWidth = 2
	opt.Padding = charts.NewBox(20, 20, 30, 20)

	painter := charts.NewPainter(charts.PainterOptions{
		OutputFormat: charts.ChartOutputPNG,
		Width:        1000,
		Height:       500,
	})
	if err := painter.LineChart(opt); err != nil {
		return nil, fmt.Errorf("render %s: %w", s.Key, err)
	}
	return painter.Bytes()
}

func usd(v float64) string {
	if v < 0 {
		return fmt.Sprintf("-$%.2f", -v)
	}
	return fmt.Sprintf("$%.2f", v)
}
