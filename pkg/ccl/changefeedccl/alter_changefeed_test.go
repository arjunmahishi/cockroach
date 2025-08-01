// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package changefeedccl

import (
	"cmp"
	"context"
	gosql "database/sql"
	"fmt"
	"maps"
	"math/rand"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/cdceval"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/cdctest"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/changefeedbase"
	"github.com/cockroachdb/cockroach/pkg/ccl/utilccl"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/server/telemetry"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/desctestutils"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

// TestAlterChangefeedAddTargetPrivileges tests permissions for
// users creating new changefeeds while altering them.
func TestAlterChangefeedAddTargetPrivileges(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()

	s, db, _ := serverutils.StartServer(t, base.TestServerArgs{
		DefaultTestTenant: base.TODOTestTenantDisabled,
		Knobs: base.TestingKnobs{
			JobsTestingKnobs: jobs.NewTestingKnobsWithShortIntervals(),
			DistSQL: &execinfra.TestingKnobs{
				Changefeed: &TestingKnobs{
					WrapSink: func(s Sink, _ jobspb.JobID) Sink {
						if _, ok := s.(*externalConnectionKafkaSink); ok {
							return s
						}
						return &externalConnectionKafkaSink{sink: s, ignoreDialError: true}
					},
				},
			},
		},
	})
	defer s.Stopper().Stop(ctx)

	rootDB := sqlutils.MakeSQLRunner(db)
	rootDB.ExecMultiple(
		t,
		`CREATE TYPE type_a as enum ('a')`,
		`CREATE TABLE table_a (id int, type type_a)`,
		`CREATE TABLE table_b (id int, type type_a)`,
		`CREATE TABLE table_c (id int, type type_a)`,
		`CREATE USER feedCreator`,
		`CREATE ROLE feedowner`,
		`GRANT feedowner TO feedCreator`,
		`GRANT SELECT ON table_a TO feedCreator`,
		`GRANT CHANGEFEED ON table_a TO feedCreator`,
		`CREATE EXTERNAL CONNECTION "first" AS 'kafka://nope'`,
		`GRANT USAGE ON EXTERNAL CONNECTION first TO feedCreator`,
		`INSERT INTO table_a(id) values (0)`,
	)

	rootDB.Exec(t, `SET CLUSTER SETTING kv.rangefeed.enabled = true`)
	enableEnterprise := utilccl.TestingDisableEnterprise()
	enableEnterprise()

	withUser := func(t *testing.T, user string, fn func(*sqlutils.SQLRunner)) {
		password := `password`
		rootDB.Exec(t, fmt.Sprintf(`ALTER USER %s WITH PASSWORD '%s'`, user, password))

		pgURL := url.URL{
			Scheme: "postgres",
			User:   url.UserPassword(user, password),
			Host:   s.SQLAddr(),
		}
		db2, err := gosql.Open("postgres", pgURL.String())
		if err != nil {
			t.Fatal(err)
		}
		defer db2.Close()
		userDB := sqlutils.MakeSQLRunner(db2)

		fn(userDB)
	}

	t.Run("using-changefeed-grant", func(t *testing.T) {
		rootDB.Exec(t, `CREATE EXTERNAL CONNECTION "second" AS 'kafka://nope'`)
		rootDB.Exec(t, `CREATE USER user1`)
		rootDB.Exec(t, `GRANT CHANGEFEED ON table_a TO user1`)

		var jobID int
		withUser(t, "feedCreator", func(userDB *sqlutils.SQLRunner) {
			row := userDB.QueryRow(t, "CREATE CHANGEFEED for table_a INTO 'external://first'")
			row.Scan(&jobID)
			userDB.Exec(t, `PAUSE JOB $1`, jobID)
			waitForJobState(userDB, t, catpb.JobID(jobID), `paused`)
			userDB.Exec(t, `ALTER JOB $1 OWNER TO feedowner`, jobID)
		})

		// user1 is missing the CHANGEFEED privilege on table_b and table_c.
		withUser(t, "user1", func(userDB *sqlutils.SQLRunner) {
			userDB.ExpectErr(t,
				"user user1 does not have privileges for job",
				fmt.Sprintf("ALTER CHANGEFEED %d ADD table_b, table_c set sink='external://second'", jobID),
			)
		})
		rootDB.Exec(t, `GRANT CHANGEFEED ON table_b TO user1`)
		withUser(t, "user1", func(userDB *sqlutils.SQLRunner) {
			userDB.ExpectErr(t,
				"user user1 does not have privileges for job",
				fmt.Sprintf("ALTER CHANGEFEED %d ADD table_b, table_c set sink='external://second'", jobID),
			)
		})
		rootDB.Exec(t, `GRANT feedowner TO user1`)
		rootDB.Exec(t, `GRANT CHANGEFEED ON table_c TO user1`)
		withUser(t, "user1", func(userDB *sqlutils.SQLRunner) {
			userDB.Exec(t,
				fmt.Sprintf("ALTER CHANGEFEED %d ADD table_b, table_c set sink='external://second'", jobID),
			)
		})

		// With require_external_connection_sink enabled, the user requires USAGE on the external connection.
		rootDB.Exec(t, "SET CLUSTER SETTING changefeed.permissions.require_external_connection_sink.enabled = true")
		withUser(t, "user1", func(userDB *sqlutils.SQLRunner) {
			userDB.ExpectErr(t,
				"user user1 does not have USAGE privilege on external_connection second",
				fmt.Sprintf("ALTER CHANGEFEED %d ADD table_b, table_c set sink='external://second'", jobID),
			)
		})
		rootDB.Exec(t, `GRANT USAGE ON EXTERNAL CONNECTION second TO user1`)
		withUser(t, "user1", func(userDB *sqlutils.SQLRunner) {
			userDB.Exec(t,
				fmt.Sprintf("ALTER CHANGEFEED %d ADD table_b, table_c set sink='external://second'", jobID),
			)
		})
		rootDB.Exec(t, "SET CLUSTER SETTING changefeed.permissions.require_external_connection_sink.enabled = false")
	})

	// TODO(#94757): remove CONTROLCHANGEFEED entirely
	t.Run("using-controlchangefeed-roleoption", func(t *testing.T) {
		rootDB.Exec(t, `CREATE USER user2 WITH CONTROLCHANGEFEED`)
		rootDB.Exec(t, `GRANT CHANGEFEED ON table_a TO user2`)
		rootDB.Exec(t, `GRANT SELECT ON table_a TO user2`)

		var jobID int
		withUser(t, "feedCreator", func(userDB *sqlutils.SQLRunner) {
			row := userDB.QueryRow(t, "CREATE CHANGEFEED for table_a INTO 'kafka://foo'")
			row.Scan(&jobID)
			userDB.Exec(t, `PAUSE JOB $1`, jobID)
			waitForJobState(userDB, t, catpb.JobID(jobID), `paused`)
			userDB.Exec(t, `ALTER JOB $1 OWNER TO feedowner`, jobID)
		})

		// user2 is missing the SELECT privilege on table_b and table_c.
		withUser(t, "user2", func(userDB *sqlutils.SQLRunner) {
			userDB.ExpectErr(t,
				"pq: user user2 does not have privileges for job",
				fmt.Sprintf("ALTER CHANGEFEED %d ADD table_b, table_c set sink='kafka://bar'", jobID),
			)
		})
		rootDB.Exec(t, `GRANT SELECT ON table_b TO user2`)
		withUser(t, "user2", func(userDB *sqlutils.SQLRunner) {
			userDB.ExpectErr(t,
				"pq: user user2 does not have privileges for job",
				fmt.Sprintf("ALTER CHANGEFEED %d ADD table_b, table_c set sink='kafka://bar'", jobID),
			)
		})
		rootDB.Exec(t, `GRANT feedowner TO user2`)
		withUser(t, "user2", func(userDB *sqlutils.SQLRunner) {
			userDB.ExpectErr(t,
				"requires the SELECT privilege on all target tables",
				fmt.Sprintf("ALTER CHANGEFEED %d ADD table_b, table_c set sink='kafka://bar'", jobID),
			)
		})
		rootDB.Exec(t, `GRANT SELECT ON table_c TO user2`)
		withUser(t, "user2", func(userDB *sqlutils.SQLRunner) {
			userDB.Exec(t,
				fmt.Sprintf("ALTER CHANGEFEED %d ADD table_b, table_c set sink='kafka://bar'", jobID),
			)
		})
	})
}

func TestAlterChangefeedAddTarget(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `CREATE TABLE bar (a INT PRIMARY KEY)`)

		testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo`)
		defer closeFeed(t, testFeed)

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		sqlDB.Exec(t, `PAUSE JOB $1`, feed.JobID())
		waitForJobState(sqlDB, t, feed.JobID(), `paused`)

		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d ADD bar`, feed.JobID()))

		sqlDB.Exec(t, fmt.Sprintf(`RESUME JOB %d`, feed.JobID()))
		waitForJobState(sqlDB, t, feed.JobID(), `running`)

		sqlDB.Exec(t, `INSERT INTO foo VALUES(1)`)
		assertPayloads(t, testFeed, []string{
			`foo: [1]->{"after": {"a": 1}}`,
		})

		sqlDB.Exec(t, `INSERT INTO bar VALUES(2)`)
		assertPayloads(t, testFeed, []string{
			`bar: [2]->{"after": {"a": 2}}`,
		})
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection)
}

// TestAlterChangefeedAddTargetAfterInitialScan tests adding a new target
// after the changefeed has already completed its initial scan.
func TestAlterChangefeedAddTargetAfterInitialScan(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testutils.RunValues(t, "initial_scan", []string{"yes", "no", "only"}, func(t *testing.T, initialScan string) {
		testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
			sqlDB := sqlutils.MakeSQLRunner(s.DB)
			sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
			sqlDB.Exec(t, `CREATE TABLE bar (a INT PRIMARY KEY, b INT)`)

			testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo`)
			defer closeFeed(t, testFeed)

			feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
			require.True(t, ok)

			checkHighwaterAdvance := func(ts hlc.Timestamp) func() error {
				return func() error {
					hw, err := feed.HighWaterMark()
					if err != nil {
						return err
					}
					if hw.After(ts) {
						return nil
					}
					return errors.Newf("waiting for highwater to advance past %s", ts)
				}
			}

			// Insert and update row into new table after changefeed was already created.
			sqlDB.Exec(t, `INSERT INTO bar VALUES(2, 2)`)
			sqlDB.Exec(t, `UPDATE bar SET b = 9 WHERE a = 2`)

			var tsStr string
			sqlDB.QueryRow(t, `INSERT INTO foo VALUES(1) RETURNING cluster_logical_timestamp()`).Scan(&tsStr)
			assertPayloads(t, testFeed, []string{
				`foo: [1]->{"after": {"a": 1}}`,
			})
			ts := parseTimeToHLC(t, tsStr)
			testutils.SucceedsSoon(t, checkHighwaterAdvance(ts))

			sqlDB.Exec(t, `PAUSE JOB $1`, feed.JobID())
			waitForJobState(sqlDB, t, feed.JobID(), `paused`)

			sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d ADD bar WITH initial_scan = '%s'`, feed.JobID(), initialScan))

			sqlDB.Exec(t, `RESUME JOB $1`, feed.JobID())
			waitForJobState(sqlDB, t, feed.JobID(), `running`)

			// Updates for the new table only start at the changefeed's current
			// highwater at the time of the ALTER CHANGEFEED.
			switch initialScan {
			case "yes":
				assertPayloads(t, testFeed, []string{
					// There is no `bar: [2]->{"after": {"a": 2, "b": 2}}` message
					// because it was inserted before the highwater.
					`bar: [2]->{"after": {"a": 2, "b": 9}}`,
				})
			case "only":
				// Strangely, when initial_scan = 'only', we don't do an initial
				// scan unless the original changefeed was initial_scan = 'only'.
			case "no":
			default:
				t.Fatalf("unknown initial scan type %q", initialScan)
			}

			sqlDB.QueryRow(t, `INSERT INTO foo VALUES(2)`)
			assertPayloads(t, testFeed, []string{
				`foo: [2]->{"after": {"a": 2}}`,
			})

			sqlDB.Exec(t, `UPDATE bar SET b = 25 WHERE a = 2`)
			assertPayloads(t, testFeed, []string{
				`bar: [2]->{"after": {"a": 2, "b": 25}}`,
			})
		}

		cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection)
	})
}

func TestAlterChangefeedAddTargetFamily(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	require.NoError(t, log.SetVModule("helpers_test=1"))

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING, FAMILY onlya (a), FAMILY onlyb (b))`)

		testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo FAMILY onlya`)
		defer closeFeed(t, testFeed)

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		tsr := sqlDB.QueryRow(t, `INSERT INTO foo VALUES(42, 'hello') RETURNING cluster_logical_timestamp()`)
		var insertTsDecStr string
		tsr.Scan(&insertTsDecStr)
		insertTs := parseTimeToHLC(t, insertTsDecStr)
		assertPayloads(t, testFeed, []string{
			`foo.onlya: [42]->{"after": {"a": 42}}`,
		})

		// Wait for the high water mark (aka resolved ts) to advance past the row we inserted's
		// mvcc ts. Otherwise, we'd see [42] again due to a catch up scan, and it
		// would muddy the waters.
		testutils.SucceedsSoon(t, func() error {
			registry := s.Server.JobRegistry().(*jobs.Registry)
			job, err := registry.LoadJob(context.Background(), feed.JobID())
			require.NoError(t, err)
			prog := job.Progress()
			if p := prog.GetHighWater(); p != nil && !p.IsEmpty() && insertTs.Less(*p) {
				return nil
			}
			return errors.New("waiting for highwater")
		})

		sqlDB.Exec(t, `PAUSE JOB $1`, feed.JobID())
		waitForJobState(sqlDB, t, feed.JobID(), `paused`)

		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d ADD foo FAMILY onlyb`, feed.JobID()))

		sqlDB.Exec(t, fmt.Sprintf(`RESUME JOB %d`, feed.JobID()))
		waitForJobState(sqlDB, t, feed.JobID(), `running`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES(37, 'goodbye')`)
		assertPayloads(t, testFeed, []string{
			// Note that we don't see foo.onlyb.[42] here, because we're not
			// doing a catchup scan and we've already processed that tuple.
			`foo.onlya: [37]->{"after": {"a": 37}}`,
			`foo.onlyb: [37]->{"after": {"b": "goodbye"}}`,
		})
	}

	// TODO: Figure out why this freezes on other sinks (ex: webhook)
	cdcTest(t, testFn, feedTestForceSink("kafka"), feedTestNoExternalConnection)
}

func TestAlterChangefeedSwitchFamily(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	require.NoError(t, log.SetVModule("helpers_test=1"))

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING, FAMILY onlya (a), FAMILY onlyb (b))`)

		testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo FAMILY onlya`)
		defer closeFeed(t, testFeed)

		tsr := sqlDB.QueryRow(t, `INSERT INTO foo VALUES(1, 'hello') RETURNING cluster_logical_timestamp()`)
		var insertTsDecStr string
		tsr.Scan(&insertTsDecStr)
		insertTs := parseTimeToHLC(t, insertTsDecStr)

		assertPayloads(t, testFeed, []string{
			`foo.onlya: [1]->{"after": {"a": 1}}`,
		})

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		// Wait for the high water mark (aka resolved ts) to advance past the row we inserted's
		// mvcc ts. Otherwise, we'd see [1] again due to a catch up scan, and it
		// would muddy the waters.
		testutils.SucceedsSoon(t, func() error {
			registry := s.Server.JobRegistry().(*jobs.Registry)
			job, err := registry.LoadJob(context.Background(), feed.JobID())
			require.NoError(t, err)
			prog := job.Progress()
			if p := prog.GetHighWater(); p != nil && !p.IsEmpty() && insertTs.Less(*p) {
				return nil
			}
			return errors.New("waiting for highwater")
		})

		sqlDB.Exec(t, `PAUSE JOB $1`, feed.JobID())
		waitForJobState(sqlDB, t, feed.JobID(), `paused`)

		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d ADD foo FAMILY onlyb DROP foo FAMILY onlya`, feed.JobID()))

		sqlDB.Exec(t, fmt.Sprintf(`RESUME JOB %d`, feed.JobID()))
		waitForJobState(sqlDB, t, feed.JobID(), `running`)

		sqlDB.Exec(t, `INSERT INTO foo VALUES(2, 'goodbye')`)
		assertPayloads(t, testFeed, []string{
			// Note that we don't see foo.onlyb.[1] here, because we're not
			// doing a catchup scan and we've already processed that tuple.
			`foo.onlyb: [2]->{"after": {"b": "goodbye"}}`,
		})
	}

	// TODO: Figure out why this freezes on other sinks (ex: cloudstorage)
	cdcTest(t, testFn, feedTestForceSink("kafka"), feedTestNoExternalConnection)
}

func TestAlterChangefeedDropTarget(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `CREATE TABLE bar (a INT PRIMARY KEY)`)

		testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo, bar`)
		defer closeFeed(t, testFeed)

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		sqlDB.Exec(t, `PAUSE JOB $1`, feed.JobID())
		waitForJobState(sqlDB, t, feed.JobID(), `paused`)

		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d DROP bar`, feed.JobID()))

		sqlDB.Exec(t, fmt.Sprintf(`RESUME JOB %d`, feed.JobID()))
		waitForJobState(sqlDB, t, feed.JobID(), `running`)

		sqlDB.Exec(t, `INSERT INTO foo VALUES(1)`)
		assertPayloads(t, testFeed, []string{
			`foo: [1]->{"after": {"a": 1}}`,
		})

		sqlDB.Exec(t, `INSERT INTO bar VALUES(2)`)
		assertPayloads(t, testFeed, nil)
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection)
}

func TestAlterChangefeedDropTargetAfterTableDrop(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `CREATE TABLE bar (a INT PRIMARY KEY)`)

		testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo, bar WITH on_error='pause'`)
		defer closeFeed(t, testFeed)

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		// Drop bar table.  This should cause the job to be paused.
		sqlDB.Exec(t, `DROP TABLE bar`)
		waitForJobState(sqlDB, t, feed.JobID(), `paused`)

		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d DROP bar`, feed.JobID()))

		sqlDB.Exec(t, fmt.Sprintf(`RESUME JOB %d`, feed.JobID()))
		waitForJobState(sqlDB, t, feed.JobID(), `running`)

		sqlDB.Exec(t, `INSERT INTO foo VALUES(1)`)
		assertPayloads(t, testFeed, []string{
			`foo: [1]->{"after": {"a": 1}}`,
		})
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection, withAllowChangefeedErr("error is expected when dropping"))
}

func TestAlterChangefeedDropTargetFamily(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING, FAMILY onlya (a), FAMILY onlyb (b))`)

		var args []any
		if _, ok := f.(*webhookFeedFactory); ok {
			args = append(args, optOutOfMetamorphicEnrichedEnvelope{reason: "metamorphic enriched envelope does not support column families for webhook sinks"})
		}
		testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo FAMILY onlya, foo FAMILY onlyb`, args...)
		defer closeFeed(t, testFeed)

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		sqlDB.Exec(t, `PAUSE JOB $1`, feed.JobID())
		waitForJobState(sqlDB, t, feed.JobID(), `paused`)

		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d DROP foo FAMILY onlyb`, feed.JobID()))

		sqlDB.Exec(t, fmt.Sprintf(`RESUME JOB %d`, feed.JobID()))
		waitForJobState(sqlDB, t, feed.JobID(), `running`)

		sqlDB.Exec(t, `INSERT INTO foo VALUES(1, 'hello')`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES(2, 'goodbye')`)
		assertPayloads(t, testFeed, []string{
			`foo.onlya: [1]->{"after": {"a": 1}}`,
			`foo.onlya: [2]->{"after": {"a": 2}}`,
		})

	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection)
}

func TestAlterChangefeedSetDiffOption(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)

		testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo with format='json'`)
		defer closeFeed(t, testFeed)

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		sqlDB.Exec(t, `PAUSE JOB $1`, feed.JobID())
		waitForJobState(sqlDB, t, feed.JobID(), `paused`)

		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d SET diff`, feed.JobID()))

		sqlDB.Exec(t, fmt.Sprintf(`RESUME JOB %d`, feed.JobID()))
		waitForJobState(sqlDB, t, feed.JobID(), `running`)

		sqlDB.Exec(t, `INSERT INTO foo VALUES (0, 'initial')`)
		assertPayloads(t, testFeed, []string{
			`foo: [0]->{"after": {"a": 0, "b": "initial"}, "before": null}`,
		})
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection)
}

func TestAlterChangefeedRespectsCDCQuery(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)

		const cdcQuery = "SELECT * FROM foo WHERE a % 2 = 0"
		testFeed := feed(t, f, "CREATE CHANGEFEED WITH format='json', envelope='wrapped' AS "+cdcQuery)
		defer closeFeed(t, testFeed)

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		sqlDB.Exec(t, `PAUSE JOB $1`, feed.JobID())
		waitForJobState(sqlDB, t, feed.JobID(), `paused`)

		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d SET diff`, feed.JobID()))
		registry := s.Server.JobRegistry().(*jobs.Registry)
		job, err := registry.LoadJob(context.Background(), feed.JobID())
		require.NoError(t, err)
		details, ok := job.Details().(jobspb.ChangefeedDetails)
		require.True(t, ok)

		// Verify the query still intact.
		sc, err := cdceval.ParseChangefeedExpression(cdcQuery)
		require.NoError(t, err)
		require.Equal(t, cdceval.AsStringUnredacted(sc), details.Select)

		// Addition/Removal of the tables is not supported yet.
		t.Run("cannot add or drop", func(t *testing.T) {
			sqlDB.ExpectErr(t, "cannot modify targets when using CDC query changefeed",
				fmt.Sprintf(`ALTER CHANGEFEED %d ADD blah SET DIFF`, feed.JobID()))
			sqlDB.ExpectErr(t, "cannot modify targets when using CDC query changefeed",
				fmt.Sprintf(`ALTER CHANGEFEED %d DROP blah SET DIFF`, feed.JobID()))
		})
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection)
}

func TestAlterChangefeedUnsetDiffOption(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)

		testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo WITH diff`)
		defer closeFeed(t, testFeed)

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		sqlDB.Exec(t, `PAUSE JOB $1`, feed.JobID())
		waitForJobState(sqlDB, t, feed.JobID(), `paused`)

		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d UNSET diff`, feed.JobID()))

		sqlDB.Exec(t, fmt.Sprintf(`RESUME JOB %d`, feed.JobID()))
		waitForJobState(sqlDB, t, feed.JobID(), `running`)

		sqlDB.Exec(t, `INSERT INTO foo VALUES (0, 'initial')`)
		assertPayloads(t, testFeed, []string{
			`foo: [0]->{"after": {"a": 0, "b": "initial"}}`,
		})
	}

	// TODO: Figure out why this fails on other sinks
	cdcTest(t, testFn, feedTestForceSink("kafka"), feedTestNoExternalConnection)
}

func TestAlterChangefeedErrors(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `CREATE TABLE bar (a INT PRIMARY KEY)`)
		testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo`)
		defer closeFeed(t, testFeed)

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		sqlDB.ExpectErr(t,
			`could not load job with job id -1`,
			`ALTER CHANGEFEED -1 ADD bar`,
		)

		sqlDB.Exec(t, `ALTER TABLE bar ADD COLUMN b INT`)
		var alterTableJobID jobspb.JobID
		sqlDB.QueryRow(t, `SELECT job_id FROM [SHOW JOBS] WHERE job_type = 'NEW SCHEMA CHANGE'`).Scan(&alterTableJobID)
		sqlDB.ExpectErr(t,
			fmt.Sprintf(`job %d is not changefeed job`, alterTableJobID),
			fmt.Sprintf(`ALTER CHANGEFEED %d ADD bar`, alterTableJobID),
		)

		sqlDB.ExpectErr(t,
			fmt.Sprintf(`job %d is not paused`, feed.JobID()),
			fmt.Sprintf(`ALTER CHANGEFEED %d ADD bar`, feed.JobID()),
		)

		sqlDB.Exec(t, `PAUSE JOB $1`, feed.JobID())
		waitForJobState(sqlDB, t, feed.JobID(), `paused`)

		sqlDB.ExpectErr(t,
			`pq: target "TABLE baz" does not exist`,
			fmt.Sprintf(`ALTER CHANGEFEED %d ADD baz`, feed.JobID()),
		)
		sqlDB.ExpectErr(t,
			`pq: target "TABLE baz" does not exist`,
			fmt.Sprintf(`ALTER CHANGEFEED %d DROP baz`, feed.JobID()),
		)
		sqlDB.ExpectErr(t,
			`pq: target "TABLE bar" already not watched by changefeed`,
			fmt.Sprintf(`ALTER CHANGEFEED %d DROP bar`, feed.JobID()),
		)
		sqlDB.ExpectErr(t,
			`pq: invalid option "qux"`,
			fmt.Sprintf(`ALTER CHANGEFEED %d SET qux`, feed.JobID()),
		)
		sqlDB.ExpectErr(t,
			`pq: cannot alter option "initial_scan"`,
			fmt.Sprintf(`ALTER CHANGEFEED %d SET initial_scan`, feed.JobID()),
		)
		sqlDB.ExpectErr(t,
			`pq: invalid option "qux"`,
			fmt.Sprintf(`ALTER CHANGEFEED %d UNSET qux`, feed.JobID()),
		)
		sqlDB.ExpectErr(t,
			`pq: cannot alter option "initial_scan"`,
			fmt.Sprintf(`ALTER CHANGEFEED %d UNSET initial_scan`, feed.JobID()),
		)
		sqlDB.ExpectErr(t,
			`pq: cannot alter option "initial_scan_only"`,
			fmt.Sprintf(`ALTER CHANGEFEED %d UNSET initial_scan_only`, feed.JobID()),
		)
		sqlDB.ExpectErr(t,
			`pq: cannot alter option "end_time"`,
			fmt.Sprintf(`ALTER CHANGEFEED %d UNSET end_time`, feed.JobID()),
		)

		sqlDB.ExpectErr(t,
			`cannot unset option "sink"`,
			fmt.Sprintf(`ALTER CHANGEFEED %d UNSET sink`, feed.JobID()),
		)

		sqlDB.ExpectErr(t,
			`pq: invalid option "diff"`,
			fmt.Sprintf(`ALTER CHANGEFEED %d ADD bar WITH diff`, feed.JobID()),
		)

		sqlDB.ExpectErr(t,
			`pq: cannot specify both "initial_scan" and "no_initial_scan"`,
			fmt.Sprintf(`ALTER CHANGEFEED %d ADD bar WITH initial_scan, no_initial_scan`, feed.JobID()),
		)

		sqlDB.ExpectErr(t, "pq: changefeed ID must be an INT value: subqueries are not allowed in cdc",
			"ALTER CHANGEFEED (SELECT 1) ADD bar")
		sqlDB.ExpectErr(t, "pq: changefeed ID must be an INT value: could not parse \"two\" as type int",
			"ALTER CHANGEFEED 'two' ADD bar")
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection)
}

func TestAlterChangefeedDropAllTargetsError(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `CREATE TABLE bar (a INT PRIMARY KEY)`)

		testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo, bar`)
		defer closeFeed(t, testFeed)

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		sqlDB.Exec(t, `PAUSE JOB $1`, feed.JobID())
		waitForJobState(sqlDB, t, feed.JobID(), `paused`)

		sqlDB.ExpectErr(t,
			`cannot drop all targets`,
			fmt.Sprintf(`ALTER CHANGEFEED %d DROP foo, bar`, feed.JobID()),
		)
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection)
}

func TestAlterChangefeedTelemetry(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO foo VALUES (1)`)
		sqlDB.Exec(t, `CREATE TABLE bar (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO bar VALUES (1)`)
		sqlDB.Exec(t, `CREATE TABLE baz (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO baz VALUES (1)`)

		// Reset the counts.
		_ = telemetry.GetFeatureCounts(telemetry.Raw, telemetry.ResetCounts)

		testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo, bar WITH diff`)
		defer closeFeed(t, testFeed)
		feed := testFeed.(cdctest.EnterpriseTestFeed)

		require.NoError(t, feed.Pause())

		// The job system clears the lease asyncronously after
		// cancellation. This lease clearing transaction can
		// cause a restart in the alter changefeed
		// transaction, which will lead to different feature
		// counter counts. Thus, we want to wait for the lease
		// clear. However, the lease clear isn't guaranteed to
		// happen, so we only wait a few seconds for it.
		waitForNoLease := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for {
				if ctx.Err() != nil {
					return
				}
				var sessionID []byte
				sqlDB.QueryRow(t, `SELECT claim_session_id FROM system.jobs WHERE id = $1`, feed.JobID()).Scan(&sessionID)
				if sessionID == nil {
					return
				}
				time.Sleep(250 * time.Millisecond)
			}
		}

		waitForNoLease()
		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d DROP bar, foo ADD baz UNSET diff SET resolved, format=json`, feed.JobID()))

		counts := telemetry.GetFeatureCounts(telemetry.Raw, telemetry.ResetCounts)
		require.Equal(t, int32(1), counts[`changefeed.alter`])
		require.Equal(t, int32(1), counts[`changefeed.alter.dropped_targets.2`])
		require.Equal(t, int32(1), counts[`changefeed.alter.added_targets.1`])
		require.Equal(t, int32(1), counts[`changefeed.alter.set_options.2`])
		require.Equal(t, int32(1), counts[`changefeed.alter.unset_options.1`])
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection)
}

// The purpose of this test is to ensure that the ALTER CHANGEFEED statement
// does not accidentally redact secret keys in the changefeed details
func TestAlterChangefeedPersistSinkURI(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	const unredactedSinkURI = "null://blah?AWS_ACCESS_KEY_ID=the_secret"

	ctx := context.Background()
	srv, rawSQLDB, _ := serverutils.StartServer(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			JobsTestingKnobs: jobs.NewTestingKnobsWithShortIntervals(),
		},
	})
	defer srv.Stopper().Stop(ctx)

	s := srv.ApplicationLayer()
	sqlDB := sqlutils.MakeSQLRunner(rawSQLDB)
	registry := s.JobRegistry().(*jobs.Registry)

	for _, l := range []serverutils.ApplicationLayerInterface{s, srv.SystemLayer()} {
		kvserver.RangefeedEnabled.Override(ctx, &l.ClusterSettings().SV, true)
	}

	query := `CREATE TABLE foo (a string)`
	sqlDB.Exec(t, query)

	query = `CREATE TABLE bar (b string)`
	sqlDB.Exec(t, query)

	var changefeedID jobspb.JobID

	doneCh := make(chan struct{})
	defer close(doneCh)
	registry.TestingWrapResumerConstructor(jobspb.TypeChangefeed,
		func(raw jobs.Resumer) jobs.Resumer {
			r := fakeResumer{
				done: doneCh,
			}
			return &r
		})

	sqlDB.QueryRow(t, `CREATE CHANGEFEED FOR TABLE foo, bar INTO $1`, unredactedSinkURI).Scan(&changefeedID)

	sqlDB.Exec(t, `PAUSE JOB $1`, changefeedID)
	waitForJobState(sqlDB, t, changefeedID, `paused`)

	sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d SET diff`, changefeedID))

	sqlDB.Exec(t, fmt.Sprintf(`RESUME JOB %d`, changefeedID))
	waitForJobState(sqlDB, t, changefeedID, `running`)

	job, err := registry.LoadJob(ctx, changefeedID)
	require.NoError(t, err)
	details, ok := job.Details().(jobspb.ChangefeedDetails)
	require.True(t, ok)

	require.Equal(t, unredactedSinkURI, details.SinkURI)
}

func TestAlterChangefeedChangeSinkTypeError(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)

		testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo`)
		defer closeFeed(t, testFeed)

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		sqlDB.Exec(t, `PAUSE JOB $1`, feed.JobID())
		waitForJobState(sqlDB, t, feed.JobID(), `paused`)

		sqlDB.ExpectErr(t,
			`pq: New sink type "null" does not match original sink type "kafka". Altering the sink type of a changefeed is disallowed, consider creating a new changefeed instead.`,
			fmt.Sprintf(`ALTER CHANGEFEED %d SET sink = 'null://'`, feed.JobID()),
		)
	}

	cdcTest(t, testFn, feedTestForceSink("kafka"), feedTestNoExternalConnection)
}

func TestAlterChangefeedChangeSinkURI(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		registry := s.Server.JobRegistry().(*jobs.Registry)
		ctx := context.Background()

		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)

		testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo`)
		defer closeFeed(t, testFeed)

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		sqlDB.Exec(t, `PAUSE JOB $1`, feed.JobID())
		waitForJobState(sqlDB, t, feed.JobID(), `paused`)

		newSinkURI := `kafka://new_kafka_uri`

		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d SET sink = '%s'`, feed.JobID(), newSinkURI))

		sqlDB.Exec(t, fmt.Sprintf(`RESUME JOB %d`, feed.JobID()))
		waitForJobState(sqlDB, t, feed.JobID(), `running`)

		job, err := registry.LoadJob(ctx, feed.JobID())
		require.NoError(t, err)
		details, ok := job.Details().(jobspb.ChangefeedDetails)
		require.True(t, ok)

		require.Equal(t, newSinkURI, details.SinkURI)
	}

	// TODO (zinger): Decide how this functionality should interact with external connections
	// and add a test for it.
	cdcTest(t, testFn, feedTestForceSink("kafka"), feedTestNoExternalConnection)
}

func TestAlterChangefeedAddTargetErrors(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO foo (a) SELECT * FROM generate_series(1, 1000)`)

		knobs := s.TestingKnobs.
			DistSQL.(*execinfra.TestingKnobs).
			Changefeed.(*TestingKnobs)

		// Ensure Scan Requests are always small enough that we receive multiple
		// resolved events during a backfill
		knobs.FeedKnobs.BeforeScanRequest = func(b *kv.Batch) error {
			b.Header.MaxSpanRequestKeys = 10
			return nil
		}

		// ensure that we do not emit a resolved timestamp
		knobs.FilterSpanWithMutation = func(r *jobspb.ResolvedSpan) (bool, error) {
			return true, nil
		}

		testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo WITH resolved = '100ms'`)

		// Kafka feeds are not buffered, so we have to consume messages.
		g := ctxgroup.WithContext(context.Background())
		g.Go(func() error {
			for {
				_, err := testFeed.Next()
				if err != nil {
					return err
				}
			}
		})
		defer func() {
			closeFeed(t, testFeed)
			_ = g.Wait()
		}()

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		require.NoError(t, feed.Pause())
		waitForJobState(sqlDB, t, feed.JobID(), `paused`)

		sqlDB.Exec(t, `CREATE TABLE bar (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO bar VALUES (1), (2), (3)`)
		sqlDB.ExpectErr(t,
			`pq: target "bar" cannot be resolved as of the creation time of the changefeed. Please wait until the high water mark progresses past the creation time of this target in order to add it to the changefeed.`,
			fmt.Sprintf(`ALTER CHANGEFEED %d ADD bar`, feed.JobID()),
		)

		// allow the changefeed to emit resolved events now
		knobs.FilterSpanWithMutation = func(r *jobspb.ResolvedSpan) (bool, error) {
			return false, nil
		}

		require.NoError(t, feed.Resume())

		// Wait for the high water mark to be non-zero.
		testutils.SucceedsSoon(t, func() error {
			registry := s.Server.JobRegistry().(*jobs.Registry)
			job, err := registry.LoadJob(context.Background(), feed.JobID())
			require.NoError(t, err)
			prog := job.Progress()
			if p := prog.GetHighWater(); p != nil && !p.IsEmpty() {
				return nil
			}
			return errors.New("waiting for highwater")
		})

		require.NoError(t, feed.Pause())
		waitForJobState(sqlDB, t, feed.JobID(), `paused`)

		sqlDB.Exec(t, `CREATE TABLE baz (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO baz VALUES (1), (2), (3)`)

		sqlDB.ExpectErr(t,
			`pq: target "baz" cannot be resolved as of the high water mark. Please wait until the high water mark progresses past the creation time of this target in order to add it to the changefeed.`,
			fmt.Sprintf(`ALTER CHANGEFEED %d ADD baz`, feed.JobID()),
		)
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection)
}

func TestAlterChangefeedDatabaseQualifiedNames(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE d.drivers (id INT PRIMARY KEY, name STRING)`)
		sqlDB.Exec(t, `CREATE TABLE d.users (id INT PRIMARY KEY, name STRING)`)
		sqlDB.Exec(t, `INSERT INTO d.drivers VALUES (1, 'Alice')`)
		sqlDB.Exec(t, `INSERT INTO d.users VALUES (1, 'Bob')`)
		testFeed := feed(t, f, `CREATE CHANGEFEED FOR d.drivers WITH resolved = '100ms', diff`)
		defer closeFeed(t, testFeed)

		assertPayloads(t, testFeed, []string{
			`drivers: [1]->{"after": {"id": 1, "name": "Alice"}, "before": null}`,
		})

		expectResolvedTimestamp(t, testFeed)

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		require.NoError(t, feed.Pause())

		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d ADD d.users WITH initial_scan UNSET diff`, feed.JobID()))

		require.NoError(t, feed.Resume())

		assertPayloads(t, testFeed, []string{
			`users: [1]->{"after": {"id": 1, "name": "Bob"}}`,
		})

		sqlDB.Exec(t, `INSERT INTO d.drivers VALUES (3, 'Carol')`)

		assertPayloads(t, testFeed, []string{
			`drivers: [3]->{"after": {"id": 3, "name": "Carol"}}`,
		})
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection)
}

func TestAlterChangefeedDatabaseScope(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE DATABASE movr`)
		sqlDB.Exec(t, `CREATE DATABASE new_movr`)

		sqlDB.Exec(t, `CREATE TABLE movr.drivers (id INT PRIMARY KEY, name STRING)`)
		sqlDB.Exec(t, `CREATE TABLE new_movr.drivers (id INT PRIMARY KEY, name STRING)`)

		sqlDB.Exec(t,
			`INSERT INTO movr.drivers VALUES (1, 'Alice')`,
		)
		sqlDB.Exec(t,
			`INSERT INTO new_movr.drivers VALUES (1, 'Bob')`,
		)

		testFeed := feed(t, f, `CREATE CHANGEFEED FOR movr.drivers WITH diff`)
		defer closeFeed(t, testFeed)

		assertPayloads(t, testFeed, []string{
			`drivers: [1]->{"after": {"id": 1, "name": "Alice"}, "before": null}`,
		})

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		require.NoError(t, feed.Pause())

		sqlDB.Exec(t, `USE new_movr`)

		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d DROP movr.drivers ADD drivers WITH initial_scan UNSET diff`, feed.JobID()))

		require.NoError(t, feed.Resume())

		assertPayloads(t, testFeed, []string{
			`drivers: [1]->{"after": {"id": 1, "name": "Bob"}}`,
		})
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection, feedTestUseRootUserConnection)
}

func TestAlterChangefeedDatabaseScopeUnqualifiedName(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE DATABASE movr`)
		sqlDB.Exec(t, `CREATE DATABASE new_movr`)

		sqlDB.Exec(t, `CREATE TABLE movr.drivers (id INT PRIMARY KEY, name STRING)`)
		sqlDB.Exec(t, `CREATE TABLE new_movr.drivers (id INT PRIMARY KEY, name STRING)`)

		sqlDB.Exec(t,
			`INSERT INTO movr.drivers VALUES (1, 'Alice')`,
		)

		sqlDB.Exec(t, `USE movr`)
		testFeed := feed(t, f, `CREATE CHANGEFEED FOR drivers WITH diff, resolved = '100ms'`)
		defer closeFeed(t, testFeed)

		assertPayloads(t, testFeed, []string{
			`drivers: [1]->{"after": {"id": 1, "name": "Alice"}, "before": null}`,
		})

		expectResolvedTimestamp(t, testFeed)

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		require.NoError(t, feed.Pause())

		sqlDB.Exec(t, `USE new_movr`)

		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d UNSET diff`, feed.JobID()))

		require.NoError(t, feed.Resume())

		sqlDB.Exec(t,
			`INSERT INTO movr.drivers VALUES (2, 'Bob')`,
		)

		assertPayloads(t, testFeed, []string{
			`drivers: [2]->{"after": {"id": 2, "name": "Bob"}}`,
		})
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection, feedTestUseRootUserConnection)
}

func TestAlterChangefeedColumnFamilyDatabaseScope(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE DATABASE movr`)
		sqlDB.Exec(t, `CREATE TABLE movr.drivers (id INT PRIMARY KEY, name STRING, FAMILY onlyid (id), FAMILY onlyname (name))`)

		sqlDB.Exec(t,
			`INSERT INTO movr.drivers VALUES (1, 'Alice')`,
		)

		var args []any
		if _, ok := f.(*webhookFeedFactory); ok {
			args = append(args, optOutOfMetamorphicEnrichedEnvelope{reason: "metamorphic enriched envelope does not support column families for webhook sinks"})
		}
		testFeed := feed(t, f, `CREATE CHANGEFEED FOR movr.drivers WITH diff, split_column_families`, args...)
		defer closeFeed(t, testFeed)

		assertPayloads(t, testFeed, []string{
			`drivers.onlyid: [1]->{"after": {"id": 1}, "before": null}`,
			`drivers.onlyname: [1]->{"after": {"name": "Alice"}, "before": null}`,
		})

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		require.NoError(t, feed.Pause())

		sqlDB.Exec(t, `USE movr`)

		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d DROP movr.drivers ADD movr.drivers FAMILY onlyid ADD drivers FAMILY onlyname UNSET diff`, feed.JobID()))

		require.NoError(t, feed.Resume())

		sqlDB.Exec(t,
			`INSERT INTO movr.drivers VALUES (2, 'Bob')`,
		)

		assertPayloads(t, testFeed, []string{
			`drivers.onlyid: [2]->{"after": {"id": 2}}`,
			`drivers.onlyname: [2]->{"after": {"name": "Bob"}}`,
		})
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection, feedTestUseRootUserConnection)
}

func TestAlterChangefeedAlterTableName(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE DATABASE movr`)
		sqlDB.Exec(t, `CREATE TABLE movr.users (id INT PRIMARY KEY, name STRING)`)
		sqlDB.Exec(t,
			`INSERT INTO movr.users VALUES (1, 'Alice')`,
		)

		// TODO(#145927): currently the metamorphic enriched envelope system for
		// webhook uses source.table_name as the topic. This test expects the
		// topic name to be durable across table renames, which is not expected
		// to be true for source.table_name.
		var args []any
		if _, ok := f.(*webhookFeedFactory); ok {
			args = append(args, optOutOfMetamorphicEnrichedEnvelope{reason: "see comment"})
		}

		testFeed := feed(t, f, `CREATE CHANGEFEED FOR movr.users WITH diff, resolved = '100ms'`, args...)
		defer closeFeed(t, testFeed)

		assertPayloads(t, testFeed, []string{
			`users: [1]->{"after": {"id": 1, "name": "Alice"}, "before": null}`,
		})

		expectResolvedTimestamp(t, testFeed)

		waitForSchemaChange(t, sqlDB, `ALTER TABLE movr.users RENAME TO movr.riders`)

		var tsLogical string
		sqlDB.QueryRow(t, `SELECT cluster_logical_timestamp()`).Scan(&tsLogical)

		ts := parseTimeToHLC(t, tsLogical)

		// ensure that the high watermark has progressed past the time in which the
		// schema change occurred
		testutils.SucceedsSoon(t, func() error {
			resolvedTS, _ := expectResolvedTimestamp(t, testFeed)
			if resolvedTS.Less(ts) {
				return errors.New("waiting for resolved timestamp to progress past the schema change event")
			}
			return nil
		})

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		require.NoError(t, feed.Pause())

		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d UNSET diff`, feed.JobID()))

		require.NoError(t, feed.Resume())

		sqlDB.Exec(t,
			`INSERT INTO movr.riders VALUES (2, 'Bob')`,
		)
		assertPayloads(t, testFeed, []string{
			`users: [2]->{"after": {"id": 2, "name": "Bob"}}`,
		})
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection, feedTestUseRootUserConnection)
}

func TestAlterChangefeedAddTargetsDuringSchemaChangeError(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// Set verbose log to confirm whether or not we hit the same nil row issue as in #140669
	require.NoError(t, log.SetVModule("kv_feed=2,changefeed_processors=2"))

	rnd, seed := randutil.NewPseudoRand()
	t.Logf("random seed: %d", seed)

	testFn := func(t *testing.T, s TestServerWithSystem, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		usingLegacySchemaChanger := maybeDisableDeclarativeSchemaChangesForTest(t, sqlDB)
		// NB: For the `ALTER TABLE foo ADD COLUMN ... DEFAULT` schema change,
		// the expected boundary is different depending on if we are using the
		// legacy schema changer or not.
		expectedBoundaryType := jobspb.ResolvedSpan_RESTART
		if usingLegacySchemaChanger {
			expectedBoundaryType = jobspb.ResolvedSpan_BACKFILL
		}

		knobs := s.TestingKnobs.
			DistSQL.(*execinfra.TestingKnobs).
			Changefeed.(*TestingKnobs)

		sqlDB.Exec(t, `CREATE TABLE foo(val INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO foo (val) SELECT * FROM generate_series(0, 999)`)

		sqlDB.Exec(t, `CREATE TABLE bar(val INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO bar (val) SELECT * FROM generate_series(0, 999)`)

		// Ensure Scan Requests are always small enough that we receive multiple
		// resolved events during a backfill
		knobs.FeedKnobs.BeforeScanRequest = func(b *kv.Batch) error {
			b.Header.MaxSpanRequestKeys = 10
			return nil
		}

		testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo WITH resolved = '1s', no_initial_scan`)
		jobFeed := testFeed.(cdctest.EnterpriseTestFeed)
		jobRegistry := s.Server.JobRegistry().(*jobs.Registry)

		// Kafka feeds are not buffered, so we have to consume messages.
		g := ctxgroup.WithContext(context.Background())
		g.Go(func() error {
			for {
				_, err := testFeed.Next()
				if err != nil {
					return err
				}
			}
		})
		defer func() {
			closeFeed(t, testFeed)
			_ = g.Wait()
		}()

		// Ensure initial backfill completes
		waitForHighwater(t, jobFeed, jobRegistry)

		// Pause job and setup overrides to force a checkpoint
		require.NoError(t, jobFeed.Pause())

		var maxCheckpointSize int64 = 100 << 20
		// Ensure that checkpoints happen every time by setting a large checkpoint size.
		// Because setting 0 for the SpanCheckpointInterval disables checkpointing,
		// setting 1 nanosecond is the smallest possible value.
		changefeedbase.SpanCheckpointInterval.Override(
			context.Background(), &s.Server.ClusterSettings().SV, 1*time.Nanosecond)
		changefeedbase.SpanCheckpointMaxBytes.Override(
			context.Background(), &s.Server.ClusterSettings().SV, maxCheckpointSize)

		// Note the tableSpan to avoid resolved events that leave no gaps
		fooDesc := desctestutils.TestingGetPublicTableDescriptor(
			s.SystemServer.DB(), s.Codec, "d", "foo")
		tableSpan := fooDesc.PrimaryIndexSpan(keys.SystemSQLCodec)

		// FilterSpanWithMutation should ensure that once the backfill begins, the following resolved events
		// that are for that backfill (are of the timestamp right after the backfill timestamp) resolve some
		// but not all of the time, which results in a checkpoint eventually being created
		haveGaps := false
		var backfillTimestamp hlc.Timestamp
		var initialCheckpoint roachpb.SpanGroup
		var foundCheckpoint int32
		progressBackoff := jobRecordPollFrequency
		var nextProgressCheck time.Time
		knobs.FilterSpanWithMutation = func(r *jobspb.ResolvedSpan) (bool, error) {
			// Stop resolving anything after checkpoint set to avoid eventually resolving the full span
			if initialCheckpoint.Len() > 0 {
				return true, nil
			}
			t.Logf("span %s %s %s", r.Span.String(), r.BoundaryType.String(), r.Timestamp.String())

			// A backfill begins when the associated resolved event arrives, which has a
			// timestamp such that all backfill spans have a timestamp of timestamp.Next().
			if r.BoundaryType == expectedBoundaryType {
				t.Logf("setting boundary timestamp %s", r.Timestamp.String())
				backfillTimestamp = r.Timestamp
				return false, nil
			}

			// Avoid reading for the job progress too frequently. Attempting
			// to read the job record continuously in a loop may continuously
			// abort the transaction which is trying to write the job record.
			if nextProgressCheck.IsZero() || nextProgressCheck.Before(timeutil.Now()) {
				// Check if we've set a checkpoint yet
				progress := loadProgress(t, jobFeed, jobRegistry)
				if checkpoint := loadCheckpoint(t, progress); checkpoint != nil {
					initialCheckpoint = makeSpanGroupFromCheckpoint(t, checkpoint)
					atomic.StoreInt32(&foundCheckpoint, 1)
				}
				nextProgressCheck = timeutil.Now().Add(progressBackoff)
			}

			// Filter non-backfill-related spans
			if !r.Timestamp.Equal(backfillTimestamp.Next()) {
				skip := !(backfillTimestamp.IsEmpty() || r.Timestamp.LessEq(backfillTimestamp.Next()))
				t.Logf("handling span %s: %t", r.Span.String(), skip)
				// Only allow spans prior to a valid backfillTimestamp to avoid moving past the backfill
				return skip, nil
			}

			// Only allow resolving if we definitely won't have a completely resolved table
			if !r.Span.Equal(tableSpan) && haveGaps {
				skip := rnd.Intn(10) > 7
				t.Logf("handling span %s: %t", r.Span.String(), skip)
				return skip, nil
			}
			t.Logf("skipping span %s", r.Span.String())
			haveGaps = true
			return true, nil
		}

		require.NoError(t, jobFeed.Resume())
		sqlDB.Exec(t, `ALTER TABLE foo ADD COLUMN b STRING DEFAULT 'd'`)

		// Wait for a checkpoint to have been set
		testutils.SucceedsSoon(t, func() error {
			if atomic.LoadInt32(&foundCheckpoint) != 0 {
				return nil
			}
			if err := jobFeed.FetchTerminalJobErr(); err != nil {
				return err
			}
			return errors.Newf("waiting for checkpoint")
		})

		require.NoError(t, jobFeed.Pause())
		waitForJobState(sqlDB, t, jobFeed.JobID(), `paused`)

		errMsg := fmt.Sprintf(
			`pq: cannot perform initial scan on newly added targets while the checkpoint is non-empty, please unpause the changefeed and wait until the high watermark progresses past the current value %s to add these targets.`,
			backfillTimestamp.AsOfSystemTime(),
		)

		sqlDB.ExpectErr(t, errMsg, fmt.Sprintf(`ALTER CHANGEFEED %d ADD bar WITH initial_scan`, jobFeed.JobID()))
	}

	cdcTestWithSystem(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection)
}

func TestAlterChangefeedAddTargetsDuringBackfill(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	var rndMu struct {
		syncutil.Mutex
		rnd *rand.Rand
	}
	rndMu.rnd, _ = randutil.NewTestRand()
	const maxCheckpointSize = 1 << 20
	const numRowsPerTable = 1000

	testFn := func(t *testing.T, s TestServerWithSystem, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo(val INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO foo (val) SELECT * FROM generate_series(0, $1)`, numRowsPerTable-1)

		sqlDB.Exec(t, `CREATE TABLE bar(val INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO bar (val) SELECT * FROM generate_series(0, $1)`, numRowsPerTable-1)

		fooDesc := desctestutils.TestingGetPublicTableDescriptor(
			s.SystemServer.DB(), s.Codec, "d", "foo")
		fooTableSpan := fooDesc.PrimaryIndexSpan(s.Codec)

		knobs := s.TestingKnobs.
			DistSQL.(*execinfra.TestingKnobs).
			Changefeed.(*TestingKnobs)

		// Ensure Scan Requests are always small enough that we receive multiple
		// resolvedFoo events during a backfill.
		const maxBatchSize = numRowsPerTable / 5
		knobs.FeedKnobs.BeforeScanRequest = func(b *kv.Batch) error {
			rndMu.Lock()
			defer rndMu.Unlock()
			// We don't want batch sizes that are too small because they could cause
			// the initial scan to take too long, leading to the waitForHighwater
			// call below to time out. The formula below is completely arbitrary and
			// was chosen to ensure that the batch sizes aren't too small (i.e. in
			// the 1-2 digit range) but are still small enough that at least a few
			// batches will be necessary.
			b.Header.MaxSpanRequestKeys = maxBatchSize/2 + rndMu.rnd.Int63n(maxBatchSize/2)
			t.Logf("set max span request keys: %d", b.Header.MaxSpanRequestKeys)
			return nil
		}

		// Emit resolved events for the majority of spans. Be extra paranoid and ensure that
		// we have at least 1 span for which we don't emit resolvedFoo timestamp (to force checkpointing).
		haveGaps := false
		knobs.FilterSpanWithMutation = func(r *jobspb.ResolvedSpan) (bool, error) {
			rndMu.Lock()
			defer rndMu.Unlock()

			if r.Span.Equal(fooTableSpan) {
				// Do not emit resolved events for the entire table span.
				// We "simulate" large table by splitting single table span into many parts, so
				// we want to resolve those sub-spans instead of the entire table span.
				// However, we have to emit something -- otherwise the entire changefeed
				// machine would not work.
				r.Span.EndKey = fooTableSpan.Key.Next()
				return false, nil
			}
			if haveGaps {
				return rndMu.rnd.Intn(10) > 7, nil
			}
			haveGaps = true
			return true, nil
		}

		// Checkpoint progress frequently, and set the checkpoint size limit.
		changefeedbase.SpanCheckpointInterval.Override(
			context.Background(), &s.Server.ClusterSettings().SV, 1)
		changefeedbase.SpanCheckpointMaxBytes.Override(
			context.Background(), &s.Server.ClusterSettings().SV, maxCheckpointSize)

		registry := s.Server.JobRegistry().(*jobs.Registry)
		testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo WITH resolved = '100ms'`)

		g := ctxgroup.WithContext(context.Background())
		g.Go(func() error {
			// Kafka feeds are not buffered, so we have to consume messages.
			// We just want to ensure that eventually, we get all the rows from foo and bar.
			expectedValues := make([]string, 2*numRowsPerTable)
			for j := 0; j < numRowsPerTable; j++ {
				expectedValues[j] = fmt.Sprintf(`foo: [%d]->{"after": {"val": %d}}`, j, j)
				expectedValues[j+numRowsPerTable] = fmt.Sprintf(`bar: [%d]->{"after": {"val": %d}}`, j, j)
			}
			return assertPayloadsBaseErr(context.Background(), testFeed, expectedValues, false, false, nil, changefeedbase.OptEnvelopeWrapped)
		})

		defer func() {
			require.NoError(t, g.Wait())
			closeFeed(t, testFeed)
		}()

		jobFeed := testFeed.(cdctest.EnterpriseTestFeed)

		// Wait for non-nil checkpoint.
		waitForCheckpoint(t, jobFeed, registry)

		// Pause the job and read and verify the latest checkpoint information.
		require.NoError(t, jobFeed.Pause())
		progress := loadProgress(t, jobFeed, registry)
		require.NotNil(t, progress.GetChangefeed())
		h := progress.GetHighWater()
		noHighWater := h == nil || h.IsEmpty()
		require.True(t, noHighWater)

		checkpoint := makeSpanGroupFromCheckpoint(t, loadCheckpoint(t, progress))
		require.Greater(t, checkpoint.Len(), 0)

		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d ADD bar WITH initial_scan`, jobFeed.JobID()))

		// Collect spans we attempt to resolve after when we resume.
		var resolvedFoo []roachpb.Span
		knobs.FilterSpanWithMutation = func(r *jobspb.ResolvedSpan) (bool, error) {
			t.Logf("resolved span: %#v", r)
			if !r.Span.Equal(fooTableSpan) {
				resolvedFoo = append(resolvedFoo, r.Span)
			}
			return false, nil
		}

		require.NoError(t, jobFeed.Resume())

		// Wait for highwater to be set, which signifies that the initial scan is complete.
		waitForHighwater(t, jobFeed, registry)

		// At this point, highwater mark should be set, and previous checkpoint should be gone.
		progress = loadProgress(t, jobFeed, registry)
		require.Nil(t, loadCheckpoint(t, progress))

		require.NoError(t, jobFeed.Pause())

		// Verify that none of the resolvedFoo spans after resume were checkpointed.
		for _, sp := range resolvedFoo {
			require.Falsef(t, checkpoint.Contains(sp.Key), "span should not have been resolved: %s", sp)
		}
	}

	cdcTestWithSystem(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection)
}

func TestAlterChangefeedDropTargetDuringInitialScan(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	rnd, _ := randutil.NewPseudoRand()

	testFn := func(t *testing.T, s TestServerWithSystem, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		sqlDB.Exec(t, `CREATE TABLE foo(val INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO foo (val) SELECT * FROM generate_series(1, 100)`)

		sqlDB.Exec(t, `CREATE TABLE bar(val INT PRIMARY KEY)`)
		sqlDB.Exec(t, `INSERT INTO bar (val) SELECT * FROM generate_series(1, 100)`)

		fooDesc := desctestutils.TestingGetPublicTableDescriptor(
			s.SystemServer.DB(), s.Codec, "d", "foo")
		fooTableSpan := fooDesc.PrimaryIndexSpan(s.Codec)

		barDesc := desctestutils.TestingGetPublicTableDescriptor(
			s.SystemServer.DB(), s.Codec, "d", "bar")
		barTableSpan := barDesc.PrimaryIndexSpan(s.Codec)

		knobs := s.TestingKnobs.
			DistSQL.(*execinfra.TestingKnobs).
			Changefeed.(*TestingKnobs)

		// Make scan requests small enough so that we're guaranteed multiple
		// resolved events during the initial scan.
		knobs.FeedKnobs.BeforeScanRequest = func(b *kv.Batch) error {
			b.Header.MaxSpanRequestKeys = 10
			return nil
		}

		var allSpans roachpb.SpanGroup
		allSpans.Add(fooTableSpan, barTableSpan)
		var allSpansResolved atomic.Bool

		// Skip some spans for both tables so that the initial scan can't complete.
		var skippedFooSpans, skippedBarSpans roachpb.SpanGroup
		knobs.FilterSpanWithMutation = func(r *jobspb.ResolvedSpan) (bool, error) {
			defer func() {
				allSpans.Sub(r.Span)
				if allSpans.Len() == 0 {
					allSpansResolved.Store(true)
				}
			}()

			if r.Span.Equal(fooTableSpan) || r.Span.Equal(barTableSpan) ||
				skippedFooSpans.Encloses(r.Span) || skippedBarSpans.Encloses(r.Span) {
				return true, nil
			}

			if fooTableSpan.Contains(r.Span) && (skippedFooSpans.Len() == 0 || rnd.Intn(3) == 0) {
				skippedFooSpans.Add(r.Span)
				return true, nil
			}

			if barTableSpan.Contains(r.Span) && (skippedBarSpans.Len() == 0 || rnd.Intn(3) == 0) {
				skippedBarSpans.Add(r.Span)
				return true, nil
			}

			return false, nil
		}

		// Create a changefeed watching both tables.
		targets := "foo, bar"
		if rnd.Intn(2) == 0 {
			targets = "bar, foo"
		}
		testFeed := feed(t, f, fmt.Sprintf(`CREATE CHANGEFEED for %s`, targets))
		defer closeFeed(t, testFeed)

		// Wait for all spans to have been resolved.
		testutils.SucceedsSoon(t, func() error {
			if allSpansResolved.Load() {
				return nil
			}
			return errors.New("expected all spans to be resolved")
		})

		// Pause the changefeed and make sure the initial scan hasn't completed yet.
		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)
		require.NoError(t, feed.Pause())
		hw, err := feed.HighWaterMark()
		require.NoError(t, err)
		require.Zero(t, hw)

		// Alter the changefeed to stop watching the second table.
		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d DROP bar`, feed.JobID()))

		allSpans.Add(fooTableSpan)
		knobs.FilterSpanWithMutation = func(r *jobspb.ResolvedSpan) (bool, error) {
			if barTableSpan.Contains(r.Span) {
				t.Fatalf("span from dropped table should not have been resolved: %#v", r.Span)
			}
			allSpans.Sub(r.Span)
			return false, nil
		}

		require.NoError(t, feed.Resume())
		require.NoError(t, feed.WaitForHighWaterMark(hlc.Timestamp{}))
		require.Zero(t, allSpans.Len())
	}

	cdcTestWithSystem(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection)
}

func TestAlterChangefeedInitialScan(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(initialScanOption string) cdcTestFn {
		return func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
			sqlDB := sqlutils.MakeSQLRunner(s.DB)
			sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
			sqlDB.Exec(t, `INSERT INTO foo VALUES (1), (2), (3)`)
			sqlDB.Exec(t, `CREATE TABLE bar (a INT PRIMARY KEY)`)
			sqlDB.Exec(t, `INSERT INTO bar VALUES (1), (2), (3)`)

			testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo WITH resolved = '1s', no_initial_scan`)
			defer closeFeed(t, testFeed)

			expectResolvedTimestamp(t, testFeed)

			feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
			require.True(t, ok)

			sqlDB.Exec(t, `PAUSE JOB $1`, feed.JobID())
			waitForJobState(sqlDB, t, feed.JobID(), `paused`)

			sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d ADD bar WITH %s`, feed.JobID(), initialScanOption))

			sqlDB.Exec(t, fmt.Sprintf(`RESUME JOB %d`, feed.JobID()))
			waitForJobState(sqlDB, t, feed.JobID(), `running`)

			expectPayloads := initialScanOption == "initial_scan = 'yes'" || initialScanOption == "initial_scan"
			if expectPayloads {
				assertPayloads(t, testFeed, []string{
					`bar: [1]->{"after": {"a": 1}}`,
					`bar: [2]->{"after": {"a": 2}}`,
					`bar: [3]->{"after": {"a": 3}}`,
				})
			}

			sqlDB.Exec(t, `INSERT INTO bar VALUES (4)`)
			assertPayloads(t, testFeed, []string{
				`bar: [4]->{"after": {"a": 4}}`,
			})
		}
	}

	for _, initialScanOpt := range []string{
		"initial_scan = 'yes'",
		"initial_scan = 'no'",
		"initial_scan = 'only'",
		"initial_scan",
		"no_initial_scan",
	} {
		cdcTest(t, testFn(initialScanOpt), feedTestForceSink("kafka"), feedTestNoExternalConnection)
	}
}

// This test checks that the time used to get table descriptors in alter
// changefeed is the time from which changefeed will resume (check
// validateNewTargets for more info on how this time is calculated).
func TestAlterChangefeedWithOldCursorFromCreateChangefeed(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		registry := s.Server.JobRegistry().(*jobs.Registry)

		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY, b STRING)`)

		var tsLogical string
		sqlDB.QueryRow(t, `SELECT cluster_logical_timestamp()`).Scan(&tsLogical)
		cursor := parseTimeToHLC(t, tsLogical)

		testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo WITH cursor=$1`, tsLogical)
		defer closeFeed(t, testFeed)

		sqlDB.Exec(t, `INSERT INTO foo VALUES (1, 'before')`)
		assertPayloads(t, testFeed, []string{
			`foo: [1]->{"after": {"a": 1, "b": "before"}}`,
		})

		castedFeed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		testutils.SucceedsSoon(t, func() error {
			progress := loadProgress(t, castedFeed, registry)
			if hw := progress.GetHighWater(); hw != nil && cursor.LessEq(*hw) {
				return nil
			}
			return errors.New("waiting for checkpoint advance")
		})

		sqlDB.Exec(t, `PAUSE JOB $1`, castedFeed.JobID())
		waitForJobState(sqlDB, t, castedFeed.JobID(), `paused`)

		sqlDB.Exec(t, `INSERT INTO foo VALUES (2, 'after')`)

		// Simulate that a significant time has passed since the create
		// change feed command was given - if the highwater mark is not
		// used in the following alter changefeed command, then we will
		// get an error when we try to get a table descriptors using
		// cursor time.
		calculateCursor := func(currentTime *hlc.Timestamp) string {
			return "-3h"
		}
		knobs := s.TestingKnobs.DistSQL.(*execinfra.TestingKnobs).Changefeed.(*TestingKnobs)
		knobs.OverrideCursor = calculateCursor

		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d UNSET resolved`, castedFeed.JobID()))

		sqlDB.Exec(t, fmt.Sprintf(`RESUME JOB %d`, castedFeed.JobID()))
		waitForJobState(sqlDB, t, castedFeed.JobID(), `running`)

		assertPayloads(t, testFeed, []string{
			`foo: [2]->{"after": {"a": 2, "b": "after"}}`,
		})
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection)
}

// TestChangefeedJobControl tests if a user can modify and existing changefeed
// based on their privileges.
func TestAlterChangefeedAccessControl(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		ChangefeedJobPermissionsTestSetup(t, s)
		rootDB := sqlutils.MakeSQLRunner(s.DB)

		createFeed := func(stmt string) (cdctest.EnterpriseTestFeed, func()) {
			successfulFeed := feed(t, f, stmt)
			closeCf := func() {
				closeFeed(t, successfulFeed)
			}
			_, err := successfulFeed.Next()
			require.NoError(t, err)
			return successfulFeed.(cdctest.EnterpriseTestFeed), closeCf
		}

		// Create a changefeed and pause it.
		var currentFeed cdctest.EnterpriseTestFeed
		var closeCf func()
		asUser(t, f, `feedCreator`, func(_ *sqlutils.SQLRunner) {
			currentFeed, closeCf = createFeed(`CREATE CHANGEFEED FOR table_a, table_b`)
		})
		rootDB.Exec(t, "PAUSE job $1", currentFeed.JobID())
		waitForJobState(rootDB, t, currentFeed.JobID(), `paused`)
		rootDB.Exec(t, "ALTER JOB $1 OWNER TO feedowner", currentFeed.JobID())

		// Verify who can modify the existing changefeed.
		asUser(t, f, `userWithAllGrants`, func(userDB *sqlutils.SQLRunner) {
			userDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d DROP table_b`, currentFeed.JobID()))
		})
		asUser(t, f, `adminUser`, func(userDB *sqlutils.SQLRunner) {
			userDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d ADD table_b`, currentFeed.JobID()))
		})
		// jobController can access the job, but will hit an error re-creating the changefeed.
		asUser(t, f, `jobController`, func(userDB *sqlutils.SQLRunner) {
			userDB.ExpectErr(t, "pq: user jobcontroller requires the CHANGEFEED privilege on all target tables to be able to run an enterprise changefeed", fmt.Sprintf(`ALTER CHANGEFEED %d DROP table_b`, currentFeed.JobID()))
		})
		asUser(t, f, `userWithSomeGrants`, func(userDB *sqlutils.SQLRunner) {
			userDB.ExpectErr(t, "does not have privileges for job", fmt.Sprintf(`ALTER CHANGEFEED %d ADD table_b`, currentFeed.JobID()))
		})
		asUser(t, f, `regularUser`, func(userDB *sqlutils.SQLRunner) {
			userDB.ExpectErr(t, "does not have privileges for job", fmt.Sprintf(`ALTER CHANGEFEED %d ADD table_b`, currentFeed.JobID()))
		})
		closeCf()

		// No one can modify changefeeds created by admins, except for admins.
		asUser(t, f, `adminUser`, func(_ *sqlutils.SQLRunner) {
			currentFeed, closeCf = createFeed(`CREATE CHANGEFEED FOR table_a, table_b`)
		})
		asUser(t, f, `otherAdminUser`, func(userDB *sqlutils.SQLRunner) {
			userDB.Exec(t, "PAUSE job $1", currentFeed.JobID())
			require.NoError(t, currentFeed.WaitForState(func(s jobs.State) bool {
				return s == jobs.StatePaused
			}))
		})
		asUser(t, f, `userWithAllGrants`, func(userDB *sqlutils.SQLRunner) {
			userDB.ExpectErr(t, "pq: only admins can control jobs owned by other admins", fmt.Sprintf(`ALTER CHANGEFEED %d ADD table_b`, currentFeed.JobID()))
		})
		asUser(t, f, `jobController`, func(userDB *sqlutils.SQLRunner) {
			userDB.ExpectErr(t, "pq: only admins can control jobs owned by other admins", fmt.Sprintf(`ALTER CHANGEFEED %d ADD table_b`, currentFeed.JobID()))
		})
		closeCf()
	}

	// Only enterprise sinks create jobs.
	cdcTest(t, testFn, feedTestEnterpriseSinks)
}

// TestAlterChangefeedAddDropSameTarget tests adding and dropping the same
// target multiple times in a statement.
func TestAlterChangefeedAddDropSameTarget(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)
		sqlDB.Exec(t, `CREATE TABLE foo (a INT PRIMARY KEY)`)
		sqlDB.Exec(t, `CREATE TABLE bar (a INT PRIMARY KEY)`)

		testFeed := feed(t, f, `CREATE CHANGEFEED FOR foo`)
		defer closeFeed(t, testFeed)

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		// Test removing and adding the same target.
		require.NoError(t, feed.Pause())
		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d DROP foo ADD foo`, feed.JobID()))
		require.NoError(t, feed.Resume())
		sqlDB.Exec(t, `INSERT INTO foo VALUES(1)`)
		assertPayloads(t, testFeed, []string{
			`foo: [1]->{"after": {"a": 1}}`,
		})

		// Test adding and removing the same target.
		require.NoError(t, feed.Pause())
		sqlDB.Exec(t, fmt.Sprintf(`ALTER CHANGEFEED %d ADD bar DROP bar`, feed.JobID()))
		require.NoError(t, feed.Resume())
		var tsStr string
		sqlDB.QueryRow(t, `INSERT INTO bar VALUES(1)`)
		sqlDB.QueryRow(t, `INSERT INTO foo VALUES(2) RETURNING cluster_logical_timestamp()`).Scan(&tsStr)
		ts := parseTimeToHLC(t, tsStr)
		require.NoError(t, feed.WaitForHighWaterMark(ts))
		// We don't expect to see the row inserted into bar.
		assertPayloads(t, testFeed, []string{
			`foo: [2]->{"after": {"a": 2}}`,
		})

		// Test adding, removing, and adding the same target.
		require.NoError(t, feed.Pause())
		sqlDB.Exec(t, fmt.Sprintf(
			`ALTER CHANGEFEED %d ADD bar DROP bar ADD bar WITH initial_scan='yes'`, feed.JobID()))
		require.NoError(t, feed.Resume())
		sqlDB.Exec(t, `INSERT INTO bar VALUES(2)`)
		assertPayloads(t, testFeed, []string{
			// TODO(#144032): This row should be produced.
			//`bar: [1]->{"after": {"a": 1}}`,
			`bar: [2]->{"after": {"a": 2}}`,
		})
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection)
}

// TestAlterChangefeedRandomizedTargetChanges tests altering a changefeed
// with randomized adding and dropping of targets.
func TestAlterChangefeedRandomizedTargetChanges(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	require.NoError(t, log.SetVModule("helpers_test=1"))

	rnd, _ := randutil.NewPseudoRand()

	testFn := func(t *testing.T, s TestServer, f cdctest.TestFeedFactory) {
		sqlDB := sqlutils.MakeSQLRunner(s.DB)

		// The tables in this test will have the rows 0, ..., tableRowCounts[tableName]-1.
		tables := make(map[string]struct{})
		tableRowCounts := make(map[string]int)

		makeExpectedRow := func(tableName string, row int, updated hlc.Timestamp) string {
			return fmt.Sprintf(`%s: [%[2]d]->{"after": {"a": %[2]d}, "updated": "%s"}`,
				tableName, row, updated.AsOfSystemTime())
		}

		insertRowsIntoTable := func(tableName string, numRows int) []string {
			rows := make([]string, 0, numRows)
			for range numRows {
				row := tableRowCounts[tableName]
				var tsStr string
				insertStmt := fmt.Sprintf(`INSERT INTO %s VALUES (%d)`, tableName, row)
				t.Log(insertStmt)
				sqlDB.QueryRow(t,
					fmt.Sprintf(`%s RETURNING cluster_logical_timestamp()`, insertStmt),
				).Scan(&tsStr)
				ts := parseTimeToHLC(t, tsStr)
				rows = append(rows, makeExpectedRow(tableName, row, ts))
				tableRowCounts[tableName] += 1
			}
			return rows
		}

		// Create 10 tables with a single row to start.
		const numTables = 10
		t.Logf("creating %d tables", numTables)
		for i := range numTables {
			tableName := fmt.Sprintf("table%d", i)
			createStmt := fmt.Sprintf(`CREATE TABLE %s (a INT PRIMARY KEY)`, tableName)
			t.Log(createStmt)
			sqlDB.Exec(t, createStmt)
			tables[tableName] = struct{}{}
			insertRowsIntoTable(tableName, 1 /* numRows */)
		}

		// makeInitialScanRows returns the expected initial scan rows assuming
		// every row in the table will be included in the initial scan.
		makeInitialScanRows := func(newTables []string, scanTime hlc.Timestamp) []string {
			var rows []string
			for _, t := range newTables {
				for i := range tableRowCounts[t] {
					rows = append(rows, makeExpectedRow(t, i, scanTime))
				}
			}
			return rows
		}

		// Randomly select some subset of tables to be the initial changefeed targets.
		initialTables := getNFromSet(rnd, tables, 1+rnd.Intn(numTables))
		watchedTables := makeSet(initialTables)
		nonWatchedTables := setDifference(tables, watchedTables)

		// Create the changefeed.
		createStmt := fmt.Sprintf(
			`CREATE CHANGEFEED FOR %s WITH updated`, strings.Join(initialTables, ", "))
		t.Log(createStmt)
		testFeed := feed(t, f, createStmt)
		defer closeFeed(t, testFeed)

		feed, ok := testFeed.(cdctest.EnterpriseTestFeed)
		require.True(t, ok)

		d, err := feed.Details()
		require.NoError(t, err)
		statementTime := d.StatementTime
		require.NoError(t, feed.WaitForHighWaterMark(statementTime))
		assertPayloads(t, testFeed, makeInitialScanRows(initialTables, statementTime))

		const numAlters = 10
		t.Logf("will perform %d alters", numAlters)
		for i := range numAlters {
			t.Logf("performing alter #%d", i+1)

			require.NoError(t, feed.Pause())

			hw, err := feed.HighWaterMark()
			require.NoError(t, err)

			var alterStmtBuilder strings.Builder
			write := func(format string, args ...any) {
				_, err := fmt.Fprintf(&alterStmtBuilder, format, args...)
				require.NoError(t, err)
			}
			write(`ALTER CHANGEFEED %d`, feed.JobID())

			// We get the set of tables to add/drop first to ensure we are
			// selecting without replacement.
			numAdds := rnd.Intn(len(nonWatchedTables) + 1)
			numDrops := rnd.Intn(len(watchedTables))
			if numAdds == 0 && numDrops == 0 {
				t.Logf("skipping alter #%d", i+1)
				continue
			}
			adds := getNFromSet(rnd, nonWatchedTables, numAdds)
			drops := getNFromSet(rnd, watchedTables, numDrops)

			var expectedRows []string
			for len(adds) > 0 || len(drops) > 0 {
				// Randomize the order of adds and drops.
				if add := len(adds) > 0 && (len(drops) == 0 || rnd.Intn(2) == 0); add {
					addTarget := adds[0]
					adds = adds[1:]
					delete(nonWatchedTables, addTarget)
					watchedTables[addTarget] = struct{}{}

					write(` ADD %s`, addTarget)

					switch rnd.Intn(4) {
					case 0:
						write(` WITH initial_scan='yes'`)
						expectedRows = append(expectedRows, makeInitialScanRows([]string{addTarget}, hw)...)
					case 1:
						write(` WITH initial_scan='only'`)
						// We don't do an initial scan because the original
						// changefeed did not have initial_scan='only'.
					case 2:
						write(` WITH initial_scan='no'`)
					case 3:
						// The default option is initial_scan='no'.
					}
					expectedRows = append(expectedRows,
						insertRowsIntoTable(addTarget, 2 /* numRows */)...)
				} else { // Drop a target.
					dropTarget := drops[0]
					drops = drops[1:]
					delete(watchedTables, dropTarget)
					nonWatchedTables[dropTarget] = struct{}{}

					write(` DROP %s`, dropTarget)

					// Insert some more rows into the table that
					// should NOT be emitted by the changefeed.
					insertRowsIntoTable(dropTarget, 3 /* numRows */)
				}
			}
			require.Empty(t, adds)
			require.Empty(t, drops)

			alterStmt := alterStmtBuilder.String()
			t.Log(alterStmt)
			sqlDB.Exec(t, alterStmt)

			require.NoError(t, feed.Resume())

			// Wait for highwater to advance past the current time so that
			// we're sure no more rows are expected.
			var tsStr string
			sqlDB.QueryRow(t, `SELECT cluster_logical_timestamp()`).Scan(&tsStr)
			ts := parseTimeToHLC(t, tsStr)
			require.NoError(t, feed.WaitForHighWaterMark(ts))

			assertPayloads(t, testFeed, expectedRows)
		}
	}

	cdcTest(t, testFn, feedTestEnterpriseSinks, feedTestNoExternalConnection)
}

// makeSet returns a new set with the elements in the provided slice.
func makeSet[K cmp.Ordered](ks []K) map[K]struct{} {
	m := make(map[K]struct{}, len(ks))
	for _, k := range ks {
		m[k] = struct{}{}
	}
	return m
}

// setDifference returns a new set that is s - t.
func setDifference[K cmp.Ordered](s map[K]struct{}, t map[K]struct{}) map[K]struct{} {
	difference := make(map[K]struct{})
	for e := range s {
		if _, ok := t[e]; !ok {
			difference[e] = struct{}{}
		}
	}
	return difference
}

// getNFromSet returns a slice with n random elements from s.
func getNFromSet[K cmp.Ordered](rnd *rand.Rand, s map[K]struct{}, n int) []K {
	if len(s) < n {
		panic(fmt.Sprintf("not enough elements in set, wanted %d, found %d", n, len(s)))
	}
	ks := slices.Sorted(maps.Keys(s))
	rnd.Shuffle(len(ks), func(i, j int) {
		ks[i], ks[j] = ks[j], ks[i]
	})
	return ks[:n]
}
