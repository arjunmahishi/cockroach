// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package main

import (
	"log"
	"os"

	"github.com/cockroachdb/cockroach/pkg/ui/dashboard"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: dashgen <output-dir>")
	}

	outputDir := os.Args[1]
	generator := dashboard.NewGenerator()

	if err := generator.GenerateAllTypeScriptDashboards(outputDir); err != nil {
		log.Fatalf("Error generating dashboards: %v", err)
	}
}