// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package dashboard

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"gopkg.in/yaml.v2"
)

//go:embed data
var dashboardConfigs embed.FS

// TypeScriptTemplate contains the Go template for generating TypeScript dashboard files.
const TypeScriptTemplate = `// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// THIS FILE IS GENERATED. DO NOT EDIT.
// To regenerate: ./dev generate dashboards

import { AxisUnits } from "@cockroachlabs/cluster-ui";
import map from "lodash/map";
import React from "react";

import LineGraph from "src/views/cluster/components/linegraph";
import { CapacityGraphTooltip } from "src/views/cluster/containers/nodeGraphs/dashboards/graphTooltips";
import { Axis, Metric } from "src/views/shared/components/metricQuery";

import {
  GraphDashboardProps,
  nodeDisplayName,
  storeIDsForNode,
} from "./dashboardUtils";

export default function (props: GraphDashboardProps) {
  const {
    nodeIDs,
    nodeSources,
    storeSources,
    tooltipSelection,
    nodeDisplayNameByID,
    storeIDsByNodeID,
    tenantSource,
  } = props;

  return [
{{range $i, $chart := .Config.Charts}}{{if $i}},

{{end}}    <LineGraph
      title="{{$chart.Title}}"
      isKvGraph={{eq (getChartGraphType $chart) "kv"}}
      sources={{getSources $chart}}
      tenantSource={tenantSource}
      tooltip={{renderTooltip $chart.Tooltip}}
      showMetricsInTooltip={{getShowMetricsInTooltip $chart}}
      preCalcGraphSize={{getPreCalcGraphSize $chart}}
    >
      <Axis{{getAxisProps $chart.Axis}}>
{{renderMetrics $chart.Metrics}}
      </Axis>
    </LineGraph>{{end}}
  ];
}`

// TemplateData holds the data passed to the template during generation.
type TemplateData struct {
	Config DashboardConfig
}

// Generator provides dashboard generation functionality.
type Generator struct{}

// NewGenerator creates a new dashboard generator.
func NewGenerator() *Generator {
	return &Generator{}
}

// GetEmbeddedConfigs returns the embedded dashboard configuration files.
func (g *Generator) GetEmbeddedConfigs() embed.FS {
	return dashboardConfigs
}

// GenerateAllTypeScriptDashboards generates TypeScript dashboard files for all embedded YAML configurations.
func (g *Generator) GenerateAllTypeScriptDashboards(outputDir string) error {
	configFiles, err := dashboardConfigs.ReadDir("data")
	if err != nil {
		return fmt.Errorf("reading embedded configs: %w", err)
	}

	for _, file := range configFiles {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".yaml") {
			continue
		}

		yamlData, err := dashboardConfigs.ReadFile("data/" + file.Name())
		if err != nil {
			return fmt.Errorf("reading embedded file %s: %w", file.Name(), err)
		}

		dashboardName := strings.TrimSuffix(file.Name(), ".yaml")
		if err := g.GenerateTypeScript(yamlData, outputDir, dashboardName); err != nil {
			return fmt.Errorf("generating dashboard for %s: %w", file.Name(), err)
		}
	}

	return nil
}

// GenerateTypeScript generates a TypeScript dashboard file from YAML configuration.
func (g *Generator) GenerateTypeScript(yamlData []byte, outputDir, dashboardName string) error {
	var config DashboardConfig
	if err := yaml.Unmarshal(yamlData, &config); err != nil {
		return fmt.Errorf("parsing YAML: %w", err)
	}

	// Create template with helper functions
	tmpl := template.New("dashboard").Funcs(template.FuncMap{
		"getSources":              getSources,
		"renderTooltip":           renderTooltip,
		"getShowMetricsInTooltip": getShowMetricsInTooltip,
		"getPreCalcGraphSize":     getPreCalcGraphSize,
		"getAxisProps":            getAxisProps,
		"renderMetrics":           renderMetrics,
		"eq":                      eq,
		// DB Console-specific helper functions
		"getChartGraphType":            GetChartGraphType,
		"getChartSources":              GetChartSources,
		"getChartShowMetricsInTooltip": GetChartShowMetricsInTooltip,
		"getChartPreCalcGraphSize":     GetChartPreCalcGraphSize,
		"getMetricRate":                GetMetricRate,
		"getMetricPerNode":             GetMetricPerNode,
		"getMetricSourcesType":         GetMetricSourcesType,
		"getMetricAggregation":         GetMetricAggregation,
	})

	tmpl, err := tmpl.Parse(TypeScriptTemplate)
	if err != nil {
		return fmt.Errorf("parsing template: %w", err)
	}

	// Generate output filename
	outputFile := fmt.Sprintf("%s_generated.tsx", dashboardName)
	outputPath := filepath.Join(outputDir, outputFile)

	// Create output file
	outFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("creating output file: %w", err)
	}
	defer outFile.Close()

	// Execute template
	data := TemplateData{
		Config: config,
	}

	if err := tmpl.Execute(outFile, data); err != nil {
		return fmt.Errorf("executing template: %w", err)
	}

	return nil
}

// Helper functions for template

func getAxisUnits(units string) string {
	switch units {
	case "duration":
		return "AxisUnits.Duration"
	case "bytes":
		return "AxisUnits.Bytes"
	case "percentage":
		return "AxisUnits.Percentage"
	case "count":
		fallthrough
	default:
		return "AxisUnits.Count"
	}
}

func getSources(chart Chart) string {
	sources := GetChartSources(chart)
	switch sources {
	case "stores":
		return "{storeSources}"
	case "nodes":
		fallthrough
	default:
		return "{nodeSources}"
	}
}

func renderTooltip(tooltip any) string {
	switch v := tooltip.(type) {
	case string:
		if v == "capacity_graph_tooltip" {
			return "{<CapacityGraphTooltip tooltipSelection={tooltipSelection} />}"
		}
		escaped := strings.ReplaceAll(v, "{tooltipSelection}", "${tooltipSelection}")
		return fmt.Sprintf("{`%s`}", escaped)
	case map[any]any:
		text, hasText := v["text"].(string)
		note, hasNote := v["note"].(string)
		if hasText && hasNote {
			return fmt.Sprintf(`{
        <div>
          %s&nbsp;
          <em>
            %s
          </em>
        </div>
      }`, text, note)
		} else if hasText {
			return fmt.Sprintf("{`%s`}", text)
		}
	}
	return fmt.Sprintf("{`%v`}", tooltip)
}

func getShowMetricsInTooltip(chart Chart) string {
	val := GetChartShowMetricsInTooltip(chart)
	if val == nil || *val {
		return "{true}"
	}
	return "{false}"
}

func getPreCalcGraphSize(chart Chart) string {
	val := GetChartPreCalcGraphSize(chart)
	if val == nil || *val {
		return "{true}"
	}
	return "{false}"
}

func getAxisProps(axis Axis) string {
	props := fmt.Sprintf(` label="%s"`, axis.Label)
	if axis.Units != "count" {
		props += fmt.Sprintf(` units={%s}`, getAxisUnits(axis.Units))
	}
	return props
}

func renderMetrics(metrics []Metric) string {
	var result strings.Builder

	for i, metric := range metrics {
		if i > 0 {
			result.WriteString("\n")
		}

		perNode := GetMetricPerNode(metric)
		if perNode != nil && *perNode {
			result.WriteString("        {map(nodeIDs, node => (\n")
			result.WriteString("          <Metric\n")
			result.WriteString("            key={node}\n")
			result.WriteString(fmt.Sprintf("            name=\"%s\"\n", metric.Name))
			result.WriteString("            title={nodeDisplayName(nodeDisplayNameByID, node)}\n")

			sourcesType := GetMetricSourcesType(metric)
			if sourcesType == "stores_for_node" {
				result.WriteString("            sources={storeIDsForNode(storeIDsByNodeID, node)}\n")
			} else {
				result.WriteString("            sources={[node]}\n")
			}

			rate := GetMetricRate(metric)
			if rate != nil && *rate {
				result.WriteString("            nonNegativeRate\n")
			}
			aggregation := GetMetricAggregation(metric)
			if aggregation == "max" {
				result.WriteString("            downsampleMax\n")
			}

			result.WriteString("          />\n")
			result.WriteString("        ))}")
		} else {
			result.WriteString("        <Metric\n")
			result.WriteString(fmt.Sprintf("          name=\"%s\"\n", metric.Name))
			result.WriteString(fmt.Sprintf("          title=\"%s\"\n", metric.Title))

			rate := GetMetricRate(metric)
			if rate != nil && *rate {
				result.WriteString("          nonNegativeRate\n")
			}
			aggregation := GetMetricAggregation(metric)
			if aggregation == "max" {
				result.WriteString("          downsampleMax\n")
			}

			result.WriteString("        />")
		}
	}

	return result.String()
}

func eq(a, b string) string {
	if a == b {
		return "{true}"
	}
	return "{false}"
}