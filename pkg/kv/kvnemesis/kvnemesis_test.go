// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package kvnemesis

import (
	"context"
	gosql "database/sql"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/kv/kvclient/kvcoord"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/apply"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/concurrency"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvserverbase"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvserverpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvtestutils"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/storage/enginepb"
	"github.com/cockroachdb/cockroach/pkg/testutils/datapathutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/testcluster"
	"github.com/cockroachdb/cockroach/pkg/util/buildutil"
	"github.com/cockroachdb/cockroach/pkg/util/envutil"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

var defaultNumSteps = envutil.EnvOrDefaultInt("COCKROACH_KVNEMESIS_STEPS", 100)

func (cfg kvnemesisTestCfg) testClusterArgs(
	ctx context.Context, tr *SeqTracker,
) base.TestClusterArgs {
	storeKnobs := &kvserver.StoreTestingKnobs{
		AllowUnsynchronizedReplicationChanges: true,
		// Drop the clock MaxOffset to reduce commit-wait time for
		// transactions that write to global_read ranges.
		MaxOffset: 10 * time.Millisecond,
		// Make sure we know the seq for each of our writes when they come out of
		// the rangefeed. We do this via an interceptor to avoid having to change
		// RangeFeed's APIs.
		RangefeedValueHeaderFilter: func(key, endKey roachpb.Key, ts hlc.Timestamp, vh enginepb.MVCCValueHeader) {
			if seq := vh.KVNemesisSeq.Get(); seq > 0 {
				tr.Add(key, endKey, ts, seq)
			}
		},
	}

	isOurCommand := func(ba *kvpb.BatchRequest) (string, uint64, bool) {
		key := ba.Requests[0].GetInner().Header().Key
		n, err := fkE(string(key))
		if err != nil {
			return "", 0, false
		}
		return string(key), n, true
	}

	shouldInject := func(baseProb float64, key uint64, attempt int) bool {
		// Example: baseProb = 0.8
		// On attempt 1, 0.8/1 = 80% chance of catching retry.
		// On attempt 2, 0.8/2 = 40%.
		// On attempt 3, 0.8/3 = 27%.
		// And so on.
		thresh := baseProb / float64(attempt)
		// NB: it's important to include "attempt" in here so that a write to a key
		// that is unlucky enough to map to, say, 1E-9, eventually gets to
		// successfully go through.
		return rand.New(rand.NewSource(int64(attempt)+int64(key))).Float64() < thresh
	}

	storeKnobs.LeaseIndexFilter = nil
	storeKnobs.InjectReproposalError = nil

	if p := cfg.injectReproposalErrorProb; p > 0 {
		var mu syncutil.Mutex
		seen := map[string]int{}
		storeKnobs.InjectReproposalError = func(pd *kvserver.ProposalData) error {
			key, n, ok := isOurCommand(pd.Request)
			if !ok {
				return nil
			}

			mu.Lock()
			defer mu.Unlock()
			seen[key]++
			if !shouldInject(p, n, seen[key]) {
				return nil
			}
			log.Infof(context.Background(), "inserting reproposal error for %s (seen %d times)", roachpb.Key(key), seen[key])
			err := errInjected // special error that kvnemesis accepts
			return errors.Wrapf(err, "on %s at %s", pd.Request.Summary(), roachpb.Key(key))
		}
	}

	if p := cfg.invalidLeaseAppliedIndexProb; p > 0 {
		var mu syncutil.Mutex
		seen := map[string]int{}
		storeKnobs.LeaseIndexFilter = func(pd *kvserver.ProposalData) kvpb.LeaseAppliedIndex {
			key, n, ok := isOurCommand(pd.Request)
			if !ok {
				return 0
			}
			// Lease requests never assign a LAI.
			if pd.Request.IsSingleRequestLeaseRequest() {
				return 0
			}

			mu.Lock()
			defer mu.Unlock()
			seen[key]++
			if !shouldInject(p, n, seen[key]) {
				return 0
			}
			log.Infof(context.Background(), "inserting illegal lease index for %s (seen %d times)", roachpb.Key(key), seen[key])
			// LAI 1 is always going to fail because the LAI is initialized when the lease
			// comes into existence. (It's important that we pick one here that reliably
			// fails because otherwise we may accidentally regress the closed timestamp[^1][^2].
			//
			// [^1]: https://github.com/cockroachdb/cockroach/issues/70894#issuecomment-1433244880
			// [^2]: https://github.com/cockroachdb/cockroach/issues/70894#issuecomment-1881165404
			return 1
		}
	}

	if cfg.assertRaftApply {
		asserter := apply.NewAsserter()
		storeKnobs.TestingProposalSubmitFilter = func(args kvserverbase.ProposalFilterArgs) (bool, error) {
			asserter.Propose(args.RangeID, args.ReplicaID, args.CmdID, args.SeedID, args.Cmd, args.Req)
			return false /* drop */, nil
		}
		storeKnobs.TestingApplyCalledTwiceFilter = func(args kvserverbase.ApplyFilterArgs) (int, *kvpb.Error) {
			if !args.Ephemeral {
				asserter.Apply(args.RangeID, args.ReplicaID, args.CmdID, args.Entry, args.Cmd.MaxLeaseIndex,
					*args.Cmd.ClosedTimestamp)
			}
			return 0, nil
		}
		storeKnobs.AfterSnapshotApplication = func(
			desc roachpb.ReplicaDescriptor, state kvserverpb.ReplicaState, snap kvserver.IncomingSnapshot,
		) {
			asserter.ApplySnapshot(snap.Desc.RangeID, desc.ReplicaID, snap.FromReplica.ReplicaID,
				state.RaftAppliedIndex, state.RaftAppliedIndexTerm, state.LeaseAppliedIndex,
				state.RaftClosedTimestamp)
		}
	}

	st := cluster.MakeTestingClusterSettings()
	// TODO(mira): Remove this cluster setting once the default is set to true.
	kvcoord.KeepRefreshSpansOnSavepointRollback.Override(ctx, &st.SV, true)
	if cfg.leaseTypeOverride != 0 {
		kvserver.OverrideDefaultLeaseType(ctx, &st.SV, cfg.leaseTypeOverride)
	}

	if cfg.testSettings != nil {
		cfg.testSettings(ctx, st)
	}

	args := base.TestClusterArgs{
		ServerArgs: base.TestServerArgs{
			Knobs: base.TestingKnobs{
				Store: storeKnobs,
				KVClient: &kvcoord.ClientTestingKnobs{
					// Don't let DistSender split DeleteRangeUsingTombstone across range boundaries.
					// This does happen in real CRDB, but leads to separate atomic subunits, which
					// would add complexity to kvnemesis that isn't worth it. Instead, the operation
					// generator for the most part tries to avoid range-spanning requests, and the
					// ones that do end up happening get a hard error.
					OnRangeSpanningNonTxnalBatch: func(ba *kvpb.BatchRequest) *kvpb.Error {
						for _, req := range ba.Requests {
							if req.GetInner().Method() != kvpb.DeleteRange {
								continue
							}
							if req.GetDeleteRange().UseRangeTombstone == true {
								return kvpb.NewError(errDelRangeUsingTombstoneStraddlesRangeBoundary)
							}
						}
						return nil
					},
				},
			},
			Settings: st,
		},
	}

	if cfg.testArgs != nil {
		cfg.testArgs(&args)
	}

	return args
}

func randWithSeed(
	t interface {
		Logf(string, ...interface{})
		Helper()
	}, cfg kvnemesisTestCfg,
) (*rand.Rand, counter, int64) {
	t.Helper()

	var rngSource rand.Source
	seedOrZero := cfg.seedOverride
	if cfg.randSource != nil {
		rngSource = cfg.randSource
		t.Logf("using config-supplied random source, seed ignored")
	} else {
		if seedOrZero > 0 {
			rngSource = rand.NewSource(seedOrZero)
		} else {
			rngSource, seedOrZero = randutil.NewTestRandSource()
		}
		t.Logf("seed: %d", seedOrZero)
	}

	countingSource := newCountingSource(rngSource.(rand.Source64))
	return rand.New(countingSource), countingSource, seedOrZero
}

type ti interface {
	Helper()
	Logf(string, ...interface{})
}

type tBridge struct {
	ti
	ll logLogger
}

func newTBridge(t testing.TB) *tBridge {
	// NB: we're not using t.TempDir() because we want these to survive
	// on failure.
	td, err := os.MkdirTemp(datapathutils.DebuggableTempDir(), "kvnemesis")
	if err != nil {
		td = datapathutils.DebuggableTempDir()
	}
	t.Cleanup(func() {
		if t.Failed() {
			return
		}
		_ = os.RemoveAll(td)
	})
	t.Logf("kvnemesis logging to %s", td)
	return &tBridge{
		ti: t,
		ll: logLogger{
			dir: td,
		},
	}
}

func (t *tBridge) WriteFile(basename string, contents string) string {
	return t.ll.WriteFile(basename, contents)
}

type kvnemesisTestCfg struct {
	numNodes     int
	numSteps     int
	concurrency  int
	randSource   rand.Source
	seedOverride int64
	// The two knobs below inject illegal lease index errors and, for the
	// resulting reproposals, reproposal errors. The injection is stateful and
	// remembers the keys on which the commands operated, and, per key, the
	// probability is scaled down linearly based on the number of times we've
	// injected an error. In other words, this can be set to 1.0 and some amount
	// of progress would still be made.
	//
	// NB: to at least directionally preserve determinism, the rand for each dice
	// roll is seeded from the uint64 represented by the key, so this shouldn't be
	// considered truly random, but is random enough for the desired purpose.
	invalidLeaseAppliedIndexProb float64 // [0,1)
	injectReproposalErrorProb    float64 // [0,1)
	// bufferedWriteProb is the probability that an SSI transaction is configured
	// to use buffered writes. Once write buffering supports RC and SSI
	// transactions, this will apply to all transactions.
	bufferedWriteProb float64 // [0,1)

	// If enabled, set the user priority of transactions to a random value.
	randomUserPriority bool

	// If enabled, track Raft proposals and command application, and assert
	// invariants (in particular that we don't double-apply a request or
	// proposal).
	assertRaftApply bool
	// If set, overrides the default lease type for ranges.
	leaseTypeOverride roachpb.LeaseType

	// testSettings is passed the settings object used for the kvnemesis
	// TestCluster.
	testSettings func(context.Context, *cluster.Settings)

	// testArgs is passed the TestClusterArgs used to start the kvnemesis
	// TestCluster.
	testArgs func(*base.TestClusterArgs)

	// testGeneratorConfig modifies the default generator configuration. This is
	// useful if a test configuration does not yet support particular operations.
	testGeneratorConfig func(*GeneratorConfig)
}

func defaultTestConfiguration(numNodes int) kvnemesisTestCfg {
	return kvnemesisTestCfg{
		numNodes:                     numNodes,
		numSteps:                     defaultNumSteps,
		concurrency:                  5,
		seedOverride:                 0,
		invalidLeaseAppliedIndexProb: 0.2,
		injectReproposalErrorProb:    0.2,
		assertRaftApply:              true,
		randomUserPriority:           true,
	}
}

func TestKVNemesisSingleNode(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	cfg := defaultTestConfiguration(1)
	cfg.seedOverride = 0
	testKVNemesisImpl(t, cfg)
}

func TestKVNemesisSingleNode_ReproposalChaos(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	cfg := defaultTestConfiguration(1)
	cfg.seedOverride = 0
	cfg.invalidLeaseAppliedIndexProb = 0.9
	cfg.injectReproposalErrorProb = 0.5

	testKVNemesisImpl(t, cfg)
}

// TestKVNemesisMultiNode_BufferedWritesNoLockDurabilityUpgrades runs KVNemesis
// with write buffering enabled and no lock durability ugprades. We leave splits
// to be metamorphic since those are all handled in-memory.
func TestKVNemesisMultiNode_BufferedWritesNoLockDurabilityUpgrades(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	cfg := defaultTestConfiguration(3)
	cfg.seedOverride = 0
	cfg.bufferedWriteProb = 0.7
	cfg.testSettings = func(ctx context.Context, st *cluster.Settings) {
		concurrency.UnreplicatedLockReliabilityLeaseTransfer.Override(ctx, &st.SV, false)
		concurrency.UnreplicatedLockReliabilityMerge.Override(ctx, &st.SV, false)
		kvcoord.BufferedWritesEnabled.Override(ctx, &st.SV, true)
	}
	testKVNemesisImpl(t, cfg)
}

// TestKVNemesisMultiNode_BufferedWritesLockDurabilityUpgrades tests buffered
// writes with all lock durability features enabled.
func TestKVNemesisMultiNode_BufferedWritesLockDurabilityUpgrades(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	cfg := defaultTestConfiguration(3)
	cfg.seedOverride = 0
	cfg.bufferedWriteProb = 0.7
	cfg.testSettings = func(ctx context.Context, st *cluster.Settings) {
		kvcoord.BufferedWritesEnabled.Override(ctx, &st.SV, true)
		concurrency.UnreplicatedLockReliabilityLeaseTransfer.Override(ctx, &st.SV, true)
		concurrency.UnreplicatedLockReliabilityMerge.Override(ctx, &st.SV, true)
		concurrency.UnreplicatedLockReliabilitySplit.Override(ctx, &st.SV, true)
	}

	testKVNemesisImpl(t, cfg)
}

// TestKVNemesisMultiNode_BufferedWritesNoPipelining turns on buffered
// writes and turns off write pipelining.
func TestKVNemesisMultiNode_BufferedWritesNoPipelining(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	cfg := defaultTestConfiguration(3)
	cfg.seedOverride = 0
	cfg.bufferedWriteProb = 0.7
	cfg.testSettings = func(ctx context.Context, st *cluster.Settings) {
		kvcoord.BufferedWritesEnabled.Override(ctx, &st.SV, true)
		kvcoord.PipelinedWritesEnabled.Override(ctx, &st.SV, false)
		concurrency.UnreplicatedLockReliabilityLeaseTransfer.Override(ctx, &st.SV, true)
		concurrency.UnreplicatedLockReliabilityMerge.Override(ctx, &st.SV, true)
		concurrency.UnreplicatedLockReliabilitySplit.Override(ctx, &st.SV, true)
	}
	testKVNemesisImpl(t, cfg)
}

func TestKVNemesisMultiNode(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	cfg := defaultTestConfiguration(4)
	cfg.seedOverride = 0
	testKVNemesisImpl(t, cfg)
}

func TestKVNemesisMultiNode_LeaderLeases(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	cfg := defaultTestConfiguration(4)
	cfg.seedOverride = 0
	cfg.leaseTypeOverride = roachpb.LeaseLeader

	testKVNemesisImpl(t, cfg)
}

// FuzzKVNemesisSingleNode is an attempt ot make it possible to run KVNemesis
// with a coverage-guided fuzzer. It takes in []bytes as input and then uses
// this to feed all random decisions in the test.
func FuzzKVNemesisSingleNode(f *testing.F) {
	defer leaktest.AfterTest(f)()
	defer log.Scope(f).Close(f)

	// Set to > 0 to pre-generate corpus data.
	const corpusSize = 0

	cfg := defaultTestConfiguration(1)
	// I've set these to low values for now to at least get things running
	// reliably. With all default settings the test runner fails without
	// printing any useful info. I _think_ it might be the result of a
	// hard-coded 10s timeout in the go-fuzz test worker.
	cfg.numSteps = 10
	cfg.concurrency = 1

	for range corpusSize {
		rndSource := randutil.NewRecordingRandSource(rand.NewSource(randutil.NewPseudoSeed()).(rand.Source64))
		cfg.randSource = rndSource
		testKVNemesisImpl(f, cfg)
		f.Add(rndSource.Output())
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		cfg.randSource = randutil.NewFuzzRandSource(t, data)
		testKVNemesisImpl(t, cfg)
	})
}

func testKVNemesisImpl(t testing.TB, cfg kvnemesisTestCfg) {
	skip.UnderRace(t)

	if !buildutil.CrdbTestBuild {
		// `kvpb.RequestHeader` and `MVCCValueHeader` have a KVNemesisSeq field
		// that is zero-sized outside test builds. We could revisit that should
		// a need arise to run kvnemesis against production binaries.
		skip.IgnoreLint(t, "kvnemesis must be run with the crdb_test build tag")
	}

	// Can set a seed here for determinism. This works best when the seed was
	// obtained with cfg.concurrency=1.
	rng, countingSource, seed := randWithSeed(t, cfg)

	// 4 nodes so we have somewhere to move 3x replicated ranges to.
	ctx := context.Background()
	tr := &SeqTracker{}
	tc := testcluster.StartTestCluster(t, cfg.numNodes, cfg.testClusterArgs(ctx, tr))
	defer tc.Stopper().Stop(ctx)
	dbs, sqlDBs := make([]*kv.DB, cfg.numNodes), make([]*gosql.DB, cfg.numNodes)
	for i := 0; i < cfg.numNodes; i++ {
		dbs[i] = tc.Server(i).DB()
		sqlDBs[i] = tc.ServerConn(i)
	}
	sqlutils.MakeSQLRunner(sqlDBs[0]).Exec(t, `SET CLUSTER SETTING kv.rangefeed.enabled = true`)
	// Turn net/trace on, which results in real trace spans created throughout.
	// This gives kvnemesis a chance to hit NPEs related to tracing.
	sqlutils.MakeSQLRunner(sqlDBs[0]).Exec(t, `SET CLUSTER SETTING trace.debug_http_endpoint.enabled = true`)

	config := NewDefaultConfig()
	config.NumNodes = cfg.numNodes
	config.NumReplicas = 3
	config.TxnConfig.BufferedWritesProb = cfg.bufferedWriteProb
	config.TxnConfig.RandomUserPriority = cfg.randomUserPriority

	config.SeedForLogging = seed
	config.RandSourceCounterForLogging = countingSource

	if config.NumReplicas > cfg.numNodes {
		config.NumReplicas = cfg.numNodes
	}
	if cfg.testGeneratorConfig != nil {
		cfg.testGeneratorConfig(&config)
	}

	logger := newTBridge(t)
	defer dumpRaftLogsOnFailure(t, logger.ll.dir, tc.Servers)
	env := &Env{SQLDBs: sqlDBs, Tracker: tr, L: logger}
	failures, err := RunNemesis(ctx, rng, env, config, cfg.concurrency, cfg.numSteps, dbs...)

	for i := 0; i < cfg.numNodes; i++ {
		t.Logf("[%d] proposed: %d", i,
			tc.GetFirstStoreFromServer(t, i).Metrics().RaftCommandsProposed.Count())
		t.Logf("[%d] reproposed unchanged: %d", i,
			tc.GetFirstStoreFromServer(t, i).Metrics().RaftCommandsReproposed.Count())
		t.Logf("[%d] reproposed with new LAI: %d", i,
			tc.GetFirstStoreFromServer(t, i).Metrics().RaftCommandsReproposedLAI.Count())
	}

	require.NoError(t, err, `%+v`, err)
	require.Zero(t, len(failures), "kvnemesis detected failures") // they've been logged already
}

// TestRunReproductionSteps is a helper that allows quickly running a kvnemesis
// history.
func TestRunReproductionSteps(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	skip.IgnoreLint(t, "test unskipped only on demand")
	ctx := context.Background()

	const n = 1 // number of nodes

	tc := testcluster.StartTestCluster(t, n, base.TestClusterArgs{})
	db0 := tc.Server(0).DB()
	_, _ = db0, ctx

	// Paste a repro as printed by kvnemesis here.
}

func dumpRaftLogsOnFailure(t testing.TB, dir string, srvs []serverutils.TestServerInterface) {
	if !t.Failed() {
		return
	}
	d := kvtestutils.RaftLogDumper{Dir: dir}
	for _, srv := range srvs {
		require.NoError(t, srv.GetStores().(*kvserver.Stores).VisitStores(func(s *kvserver.Store) error {
			s.VisitReplicas(func(replica *kvserver.Replica) (wantMore bool) {
				d.Dump(t, s.LogEngine(), s.StoreID(), replica.RangeID)
				return true // more
			})
			return nil
		}))
	}
}
