// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package dashboard

// Helper functions for extracting DB Console-specific options from the generic Chart and Metric structs.

// GetChartGraphType extracts the graph_type option from a Chart.
func GetChartGraphType(chart Chart) string {
	if chart.Options != nil {
		if val, ok := chart.Options["graph_type"].(string); ok {
			return val
		}
	}
	return ""
}

// GetChartSources extracts the sources option from a Chart.
func GetChartSources(chart Chart) string {
	if chart.Options != nil {
		if val, ok := chart.Options["sources"].(string); ok {
			return val
		}
	}
	return "nodes"
}

// GetChartShowMetricsInTooltip extracts the show_metrics_in_tooltip option from a Chart.
func GetChartShowMetricsInTooltip(chart Chart) *bool {
	if chart.Options != nil {
		if val, ok := chart.Options["show_metrics_in_tooltip"].(bool); ok {
			return &val
		}
	}
	return nil
}

// GetChartPreCalcGraphSize extracts the precalc_graph_size option from a Chart.
func GetChartPreCalcGraphSize(chart Chart) *bool {
	if chart.Options != nil {
		if val, ok := chart.Options["precalc_graph_size"].(bool); ok {
			return &val
		}
	}
	return nil
}

// GetMetricRate extracts the rate option from a Metric.
func GetMetricRate(metric Metric) *bool {
	if metric.Options != nil {
		if val, ok := metric.Options["rate"].(bool); ok {
			return &val
		}
	}
	return nil
}

// GetMetricPerNode extracts the per_node option from a Metric.
func GetMetricPerNode(metric Metric) *bool {
	if metric.Options != nil {
		if val, ok := metric.Options["per_node"].(bool); ok {
			return &val
		}
	}
	return nil
}

// GetMetricSourcesType extracts the sources_type option from a Metric.
func GetMetricSourcesType(metric Metric) string {
	if metric.Options != nil {
		if val, ok := metric.Options["sources_type"].(string); ok {
			return val
		}
	}
	return ""
}

// GetMetricAggregation extracts the aggregation option from a Metric.
func GetMetricAggregation(metric Metric) string {
	if metric.Options != nil {
		if val, ok := metric.Options["aggregation"].(string); ok {
			return val
		}
	}
	return ""
}