package main

import (
	"fmt"
	"time"

	"github.com/go-analyze/charts"
)

const samples = 145

// Line is one curve on a chart. RightAxis plots it on the second Y axis.
type Line struct {
	Name      string
	S         *Series
	RightAxis bool
}

// MultiChart renders the lines' window PnL as one PNG line chart.
func MultiChart(title, subtitle string, lines []Line, start, end time.Time) ([]byte, error) {
	var times []time.Time
	data := make([][]float64, len(lines))
	names := make([]string, len(lines))
	for i, l := range lines {
		times, data[i] = l.S.Sample(start, end, samples)
		names[i] = l.Name
	}
	labels := make([]string, len(times))
	for i, t := range times {
		labels[i] = t.UTC().Format("01-02 15:04")
	}
	opt := charts.NewLineChartOptionWithData(data)
	axes := 1
	for i, l := range lines {
		opt.SeriesList[i].Name = l.Name
		if l.RightAxis {
			opt.SeriesList[i].YAxisIndex = 1
			axes = 2
		}
	}
	opt.Title.Text = title
	opt.Title.Subtext = subtitle
	opt.Legend.SeriesNames = names
	opt.Legend.Offset = charts.OffsetStr{Top: charts.PositionBottom}
	opt.XAxis.Labels = labels
	opt.XAxis.Unit = (samples - 1) / 6
	opt.XAxis.BoundaryGap = charts.Ptr(false)
	opt.YAxis = make([]charts.YAxisOption, axes)
	for i := range opt.YAxis {
		opt.YAxis[i] = charts.YAxisOption{ValueFormatter: func(v float64) string { return usd(v) }}
	}
	opt.Symbol.Shape = charts.SymbolNone
	opt.LineStrokeWidth = 2
	opt.Padding = charts.NewBox(20, 20, 30, 20)

	painter := charts.NewPainter(charts.PainterOptions{
		OutputFormat: charts.ChartOutputPNG,
		Width:        1000,
		Height:       600,
	})
	if err := painter.LineChart(opt); err != nil {
		return nil, fmt.Errorf("render %s: %w", title, err)
	}
	return painter.Bytes()
}

func usd(v float64) string {
	if v < 0 {
		return fmt.Sprintf("-$%.2f", -v)
	}
	return fmt.Sprintf("$%.2f", v)
}

func summary(s *Series) string {
	return fmt.Sprintf("realized PnL %s · max drawdown %s", usd(s.Metrics.PnL), usd(s.Metrics.MaxDrawdown))
}
