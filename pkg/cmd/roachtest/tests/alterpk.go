// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tests

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/cluster"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/option"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/registry"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/spec"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/test"
	"github.com/cockroachdb/cockroach/pkg/roachprod/install"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
)

func registerAlterPK(r registry.Registry) {

	setupTest := func(ctx context.Context, t test.Test, c cluster.Cluster) {
		t.Status("copying binaries")

		t.Status("starting cockroach nodes")
		c.Start(ctx, t.L(), option.DefaultStartOpts(), install.MakeClusterSettings(), c.CRDBNodes())
	}

	// runAlterPKBank runs a primary key change while the bank workload runs.
	runAlterPKBank := func(ctx context.Context, t test.Test, c cluster.Cluster) {
		const numRows = 1000000
		const duration = 3 * time.Minute

		setupTest(ctx, t, c)

		initDone := make(chan struct{}, 1)
		pkChangeDone := make(chan struct{}, 1)

		m := c.NewDeprecatedMonitor(ctx, c.CRDBNodes())
		m.Go(func(ctx context.Context) error {
			// Load up a relatively small dataset to perform a workload on.

			// Init the workload.
			cmd := fmt.Sprintf("./cockroach workload init bank --drop --rows %d {pgurl%s}", numRows, c.CRDBNodes())
			if err := c.RunE(ctx, option.WithNodes(c.WorkloadNode()), cmd); err != nil {
				t.Fatal(err)
			}
			initDone <- struct{}{}

			// Run the workload while the primary key change is happening.
			cmd = fmt.Sprintf("./cockroach workload run bank --duration=%s {pgurl%s}", duration, c.CRDBNodes())
			c.Run(ctx, option.WithNodes(c.WorkloadNode()), cmd)
			// Wait for the primary key change to finish.
			<-pkChangeDone
			t.Status("starting second run of the workload after primary key change")
			// Run the workload after the primary key change occurs.
			c.Run(ctx, option.WithNodes(c.WorkloadNode()), cmd)
			return nil
		})
		m.Go(func(ctx context.Context) error {
			// Wait for the initialization to finish. Once it's done,
			// sleep for some time, then alter the primary key.
			<-initDone
			time.Sleep(duration / 30)

			t.Status("beginning primary key change")
			defer func() { pkChangeDone <- struct{}{} }()
			db := c.Conn(ctx, t.L(), c.CRDBNodes()[0])
			defer db.Close()
			cmds := []string{
				`USE bank;`,
				`ALTER TABLE bank ALTER COLUMN balance SET NOT NULL;`,
				`ALTER TABLE bank ALTER PRIMARY KEY USING COLUMNS (id, balance)`,
			}
			conn, err := db.Conn(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			for _, cmd := range cmds {
				if _, err := conn.ExecContext(ctx, cmd); err != nil {
					return err
				}
			}
			t.Status("primary key change finished")
			return nil
		})
		m.Wait()
	}

	// runAlterPKTPCC runs a primary key change while the TPCC workload runs.
	runAlterPKTPCC := func(ctx context.Context, t test.Test, c cluster.Cluster, warehouses int, expensiveChecks bool) {
		const duration = 10 * time.Minute

		setupTest(ctx, t, c)
		cmd := fmt.Sprintf(
			"./cockroach workload fixtures import tpcc --warehouses=%d --db=tpcc {pgurl:1}",
			warehouses,
		)
		if err := c.RunE(ctx, option.WithNodes(c.Node(c.CRDBNodes()[0])), cmd); err != nil {
			t.Fatal(err)
		}

		m := c.NewDeprecatedMonitor(ctx, c.CRDBNodes())
		m.Go(func(ctx context.Context) error {
			// Start running the workload.
			runCmd := fmt.Sprintf(
				"./cockroach workload run tpcc --warehouses=%d --split --scatter --duration=%s {pgurl%s}",
				warehouses,
				duration,
				c.CRDBNodes(),
			)
			t.Status("beginning workload")
			c.Run(ctx, option.WithNodes(c.WorkloadNode()), runCmd)
			t.Status("finished running workload")
			return nil
		})
		m.Go(func(ctx context.Context) error {
			// Start a primary key change after some delay.
			time.Sleep(duration / 10)

			// Pick a random table to change the primary key of.
			alterStmts := []string{
				`ALTER TABLE warehouse ALTER PRIMARY KEY USING COLUMNS (w_id)`,
				`ALTER TABLE district ALTER PRIMARY KEY USING COLUMNS (d_w_id, d_id)`,
				`ALTER TABLE history ALTER PRIMARY KEY USING COLUMNS (h_w_id, rowid)`,
				`ALTER TABLE customer ALTER PRIMARY KEY USING COLUMNS (c_w_id, c_d_id, c_id)`,
				`ALTER TABLE "order" ALTER PRIMARY KEY USING COLUMNS (o_w_id, o_d_id, o_id DESC)`,
				`ALTER TABLE new_order ALTER PRIMARY KEY USING COLUMNS (no_w_id, no_d_id, no_o_id)`,
				`ALTER TABLE item ALTER PRIMARY KEY USING COLUMNS (i_id)`,
				`ALTER TABLE stock ALTER PRIMARY KEY USING COLUMNS (s_w_id, s_i_id)`,
				`ALTER TABLE order_line ALTER PRIMARY KEY USING COLUMNS (ol_w_id, ol_d_id, ol_o_id DESC, ol_number)`,
			}

			rand, _ := randutil.NewTestRand()
			randStmt := alterStmts[rand.Intn(len(alterStmts))]
			t.Status("Running command: ", randStmt)

			db := c.Conn(ctx, t.L(), c.CRDBNodes()[0])
			defer db.Close()
			alterCmd := `USE tpcc; %s;`
			t.Status("beginning primary key change")
			if _, err := db.ExecContext(ctx, fmt.Sprintf(alterCmd, randStmt)); err != nil {
				t.Fatal(err)
			}
			t.Status("primary key change finished")
			return nil
		})

		m.Wait()

		// Run the verification checks of the TPCC workload post primary key change.
		expensiveChecksArg := ""
		if expensiveChecks {
			expensiveChecksArg = "--expensive-checks"
		}
		checkCmd := fmt.Sprintf(
			"./cockroach workload check tpcc --warehouses %d %s {pgurl%s}",
			warehouses,
			expensiveChecksArg,
			c.Node(1),
		)
		t.Status("beginning database verification")
		c.Run(ctx, option.WithNodes(c.WorkloadNode()), checkCmd)
		t.Status("finished database verification")
	}
	r.Add(registry.TestSpec{
		Name:  "alterpk-bank",
		Owner: registry.OwnerSQLFoundations,
		// Use a 4 node cluster -- 3 nodes will run cockroach, and the last will be the
		// workload driver node.
		Cluster:          r.MakeClusterSpec(4, spec.WorkloadNode()),
		Leases:           registry.MetamorphicLeases,
		CompatibleClouds: registry.AllExceptAWS,
		Suites:           registry.Suites(registry.Nightly),
		Run:              runAlterPKBank,
	})
	r.Add(registry.TestSpec{
		Name:  "alterpk-tpcc-250",
		Owner: registry.OwnerSQLFoundations,
		// Use a 4 node cluster -- 3 nodes will run cockroach, and the last will be the
		// workload driver node.
		Cluster:          r.MakeClusterSpec(4, spec.CPU(32), spec.WorkloadNode()),
		Leases:           registry.MetamorphicLeases,
		CompatibleClouds: registry.AllExceptAWS,
		Suites:           registry.Suites(registry.Nightly),
		Run: func(ctx context.Context, t test.Test, c cluster.Cluster) {
			runAlterPKTPCC(ctx, t, c, 250 /* warehouses */, true /* expensiveChecks */)
		},
	})
	r.Add(registry.TestSpec{
		Name:  "alterpk-tpcc-500",
		Owner: registry.OwnerSQLFoundations,
		// Use a 4 node cluster -- 3 nodes will run cockroach, and the last will be the
		// workload driver node.
		Cluster:          r.MakeClusterSpec(4, spec.CPU(16), spec.WorkloadNode()),
		Leases:           registry.MetamorphicLeases,
		CompatibleClouds: registry.AllExceptAWS,
		Suites:           registry.Suites(registry.Nightly),
		Run: func(ctx context.Context, t test.Test, c cluster.Cluster) {
			runAlterPKTPCC(ctx, t, c, 500 /* warehouses */, false /* expensiveChecks */)
		},
	})
}
