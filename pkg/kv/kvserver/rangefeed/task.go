// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package rangefeed

import (
	"context"
	"time"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/concurrency/lock"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/storage/enginepb"
	"github.com/cockroachdb/cockroach/pkg/storage/fs"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
)

// A runnable can be run as an async task.
type runnable interface {
	// Run executes the runnable. Cannot be called multiple times.
	Run(context.Context)
	// Cancel must be called if runnable is not Run.
	Cancel()
}

// processorTaskHelper abstracts away processor for tasks.
type processorTaskHelper interface {
	StopWithErr(pErr *kvpb.Error)
	setResolvedTSInitialized(ctx context.Context)
	sendEvent(ctx context.Context, e event, timeout time.Duration) bool
}

// initResolvedTSScan scans over all keys using the provided iterator and
// informs the rangefeed Processor of any intents. This allows the Processor to
// backfill its unresolvedIntentQueue with any intents that were written before
// the Processor was started and hooked up to a stream of logical operations.
// The Processor can initialize its resolvedTimestamp once the scan completes
// because it knows it is now tracking all intents in its key range.
type initResolvedTSScan struct {
	span roachpb.RSpan
	p    processorTaskHelper
	is   IntentScanner
}

func newInitResolvedTSScan(span roachpb.RSpan, p processorTaskHelper, c IntentScanner) runnable {
	return &initResolvedTSScan{span: span, p: p, is: c}
}

func (s *initResolvedTSScan) Run(ctx context.Context) {
	defer s.Cancel()
	if err := s.iterateAndConsume(ctx); err != nil {
		err = errors.Wrap(err, "initial resolved timestamp scan failed")
		if ctx.Err() == nil { // cancellation probably caused the error
			log.Errorf(ctx, "%v", err)
		}
		s.p.StopWithErr(kvpb.NewError(err))
	} else {
		// Inform the processor that its resolved timestamp can be initialized.
		s.p.setResolvedTSInitialized(ctx)
	}
}

func (s *initResolvedTSScan) iterateAndConsume(ctx context.Context) error {
	startKey := s.span.Key.AsRawKey()
	endKey := s.span.EndKey.AsRawKey()
	return s.is.ConsumeIntents(ctx, startKey, endKey, func(op enginepb.MVCCWriteIntentOp) bool {
		var ops [1]enginepb.MVCCLogicalOp
		ops[0].SetValue(&op)
		return s.p.sendEvent(ctx, event{ops: ops[:]}, 0)
	})
}

func (s *initResolvedTSScan) Cancel() {
	s.is.Close()
}

type eventConsumer func(enginepb.MVCCWriteIntentOp) bool

// IntentScanner is used by the ResolvedTSScan to find all intents on
// a range.
type IntentScanner interface {
	// ConsumeIntents calls consumer on any intents found on keys between startKey and endKey.
	ConsumeIntents(ctx context.Context, startKey roachpb.Key, endKey roachpb.Key, consumer eventConsumer) error
	// Close closes the IntentScanner.
	Close()
}

// SeparatedIntentScanner is an IntentScanner that scans the lock table keyspace
// and searches for intents.
type SeparatedIntentScanner struct {
	iter *storage.LockTableIterator
}

// NewSeparatedIntentScanner returns an IntentScanner appropriate for
// use when the separated intents migration has completed.
func NewSeparatedIntentScanner(
	ctx context.Context, reader storage.Reader, span roachpb.RSpan,
) (IntentScanner, error) {
	lowerBound, _ := keys.LockTableSingleKey(span.Key.AsRawKey(), nil)
	upperBound, _ := keys.LockTableSingleKey(span.EndKey.AsRawKey(), nil)
	iter, err := storage.NewLockTableIterator(
		// Do not use ctx, since it is not the ctx passed in when ConsumeIntents
		// is called. See https://github.com/cockroachdb/cockroach/issues/116440.
		//
		// NB: the storage iterator does not respect context cancellation, and
		// only uses it for tracing.
		context.Background(), reader, storage.LockTableIteratorOptions{
			LowerBound: lowerBound,
			UpperBound: upperBound,
			// Ignore Shared and Exclusive locks. We only care about intents.
			MatchMinStr:  lock.Intent,
			ReadCategory: fs.RangefeedReadCategory,
		})
	if err != nil {
		return nil, err
	}
	return &SeparatedIntentScanner{iter: iter}, nil
}

// ConsumeIntents implements the IntentScanner interface.
func (s *SeparatedIntentScanner) ConsumeIntents(
	ctx context.Context, startKey roachpb.Key, _ roachpb.Key, consumer eventConsumer,
) error {
	ltStart, _ := keys.LockTableSingleKey(startKey, nil)
	var meta enginepb.MVCCMetadata
	// TODO(sumeer): ctx is not used for iteration. Fix by adding a method to
	// EngineIterator to replace the context.
	for valid, err := s.iter.SeekEngineKeyGE(storage.EngineKey{Key: ltStart}); ; valid, err = s.iter.NextEngineKey() {
		if err != nil {
			return err
		} else if !valid {
			// We depend on the iterator having an
			// UpperBound set and becoming invalid when it
			// hits the UpperBound.
			break
		}

		engineKey, err := s.iter.UnsafeEngineKey()
		if err != nil {
			return err
		}
		ltKey, err := engineKey.ToLockTableKey()
		if err != nil {
			return errors.Wrapf(err, "decoding LockTable key: %s", ltKey)
		}
		if ltKey.Strength != lock.Intent {
			return errors.AssertionFailedf("LockTableKey with strength %s: %s", ltKey.Strength, ltKey)
		}

		v, err := s.iter.UnsafeValue()
		if err != nil {
			return err
		}
		if err := protoutil.Unmarshal(v, &meta); err != nil {
			return errors.Wrapf(err, "unmarshaling mvcc meta for locked key %s", ltKey)
		}
		if meta.Txn == nil {
			return errors.Newf("expected transaction metadata but found none for %s", ltKey)
		}

		consumer(enginepb.MVCCWriteIntentOp{
			TxnID:           meta.Txn.ID,
			TxnKey:          meta.Txn.Key,
			TxnIsoLevel:     meta.Txn.IsoLevel,
			TxnMinTimestamp: meta.Txn.MinTimestamp,
			Timestamp:       meta.Txn.WriteTimestamp,
		})
	}
	return nil
}

// Close implements the IntentScanner interface.
func (s *SeparatedIntentScanner) Close() { s.iter.Close() }

// TxnPusher is capable of pushing transactions to a new timestamp and
// cleaning up the intents of transactions that are found to be committed.
type TxnPusher interface {
	// PushTxns attempts to push the specified transactions to a new
	// timestamp. It returns the resulting transaction protos, and a
	// bool indicating whether any txn aborts were ambiguous (see
	// PushTxnResponse.AmbiguousAbort).
	//
	// NB: anyAmbiguousAbort may be false with nodes <24.1.
	PushTxns(context.Context, []enginepb.TxnMeta, hlc.Timestamp) ([]*roachpb.Transaction, bool, error)
	// ResolveIntents resolves the specified intents.
	ResolveIntents(ctx context.Context, intents []roachpb.LockUpdate) error
	// Barrier waits for all past and ongoing write commands in the range to have
	// applied on the leaseholder and the local replica.
	Barrier(ctx context.Context) error
}

// txnPushAttempt pushes all old transactions that have unresolved intents on
// the range which are blocking the resolved timestamp from moving forward. It
// does so in two steps.
//  1. it pushes all old transactions to the current timestamp and gathers
//     up the transactions' authoritative transaction records.
//  2. for each transaction that is pushed, it checks the transaction's current
//     status and reacts accordingly:
//     - PENDING:   inform the Processor that the transaction's timestamp has
//     increased so that the transaction's intents no longer need
//     to block the resolved timestamp. Even though the intents
//     may still be at an older timestamp, we know that they can't
//     commit at that timestamp.
//     - COMMITTED: launch async processes to resolve the transaction's intents
//     so they will be resolved sometime soon and unblock the
//     resolved timestamp.
//     - ABORTED:   inform the Processor to stop caring about the transaction.
//     It will never commit and its intents can be safely ignored.
type txnPushAttempt struct {
	st     *cluster.Settings
	span   roachpb.RSpan
	pusher TxnPusher
	p      processorTaskHelper
	txns   []enginepb.TxnMeta
	ts     hlc.Timestamp
	done   func()
}

func newTxnPushAttempt(
	st *cluster.Settings,
	span roachpb.RSpan,
	pusher TxnPusher,
	p processorTaskHelper,
	txns []enginepb.TxnMeta,
	ts hlc.Timestamp,
	done func(),
) runnable {
	return &txnPushAttempt{
		st:     st,
		span:   span,
		pusher: pusher,
		p:      p,
		txns:   txns,
		ts:     ts,
		done:   done,
	}
}

func (a *txnPushAttempt) Run(ctx context.Context) {
	defer a.Cancel()
	if err := a.pushOldTxns(ctx); err != nil {
		if ctx.Err() == nil { // cancellation probably caused the error
			log.Errorf(ctx, "pushing old intents failed: %v", err)
		}
	}
}

func (a *txnPushAttempt) pushOldTxns(ctx context.Context) error {
	// Push all transactions using the TxnPusher to the current time.
	// This may cause transaction restarts, but span refreshing should
	// prevent a restart for any transaction that has not been written
	// over at a larger timestamp.
	pushedTxns, anyAmbiguousAbort, err := a.pusher.PushTxns(ctx, a.txns, a.ts)
	if err != nil {
		return err
	}
	if len(pushedTxns) != len(a.txns) {
		// We expect results for all txns. In particular, if no txns have been pushed, we'd
		// crash later cause we'd be creating an invalid empty event.
		return errors.AssertionFailedf("tried to push %d transactions, got response for %d",
			len(a.txns), len(pushedTxns))
	}

	// Inform the Processor of the results of the push for each transaction.
	ops := make([]enginepb.MVCCLogicalOp, len(pushedTxns))
	var intentsToCleanup []roachpb.LockUpdate
	for i, txn := range pushedTxns {
		switch txn.Status {
		case roachpb.PENDING, roachpb.PREPARED, roachpb.STAGING:
			// The transaction is still in progress but its timestamp was moved
			// forward to the current time. Inform the Processor that it can
			// forward the txn's timestamp in its unresolvedIntentQueue.
			ops[i].SetValue(&enginepb.MVCCUpdateIntentOp{
				TxnID:     txn.ID,
				Timestamp: txn.WriteTimestamp,
			})
		case roachpb.COMMITTED:
			// The transaction is committed and its timestamp may have moved
			// forward since we last saw an intent. Inform the Processor
			// immediately in case this is the transaction that is holding back
			// the resolved timestamp. However, we still need to wait for the
			// transaction's intents to actually be resolved.
			ops[i].SetValue(&enginepb.MVCCUpdateIntentOp{
				TxnID:     txn.ID,
				Timestamp: txn.WriteTimestamp,
			})

			// Clean up the transaction's intents within the processor's range, which
			// should eventually cause all unresolved intents for this transaction on
			// the rangefeed's range to be resolved. We'll have to wait until the
			// intents are resolved before the resolved timestamp can advance past the
			// transaction's commit timestamp, so the best we can do is help speed up
			// the resolution.
			txnIntents := intentsInBound(txn, a.span.AsRawSpanWithNoLocals())
			intentsToCleanup = append(intentsToCleanup, txnIntents...)
		case roachpb.ABORTED:
			// The transaction is aborted, so it doesn't need to be tracked
			// anymore nor does it need to prevent the resolved timestamp from
			// advancing. Inform the Processor that it can remove the txn from
			// its unresolvedIntentQueue.
			//
			// NOTE: the unresolvedIntentQueue will ignore MVCCAbortTxn operations
			// before it has been initialized. This is not a concern here though
			// because we never launch txnPushAttempt tasks before the queue has
			// been initialized.
			ops[i].SetValue(&enginepb.MVCCAbortTxnOp{
				TxnID: txn.ID,
			})

			// We just informed the Processor about this txn being aborted, so from
			// its perspective, there's nothing more to do — the txn's intents are no
			// longer holding up the resolved timestamp.
			//
			// However, if the txn happens to have its LockSpans populated, then lets
			// clean up the intents within the processor's range as an optimization to
			// help others and to prevent any rangefeed reconnections from needing to
			// push the same txn. If we aborted the txn, then it won't have its
			// LockSpans populated. If, however, we ran into a transaction that its
			// coordinator tried to rollback but didn't follow up with garbage
			// collection, then LockSpans will be populated.
			txnIntents := intentsInBound(txn, a.span.AsRawSpanWithNoLocals())
			intentsToCleanup = append(intentsToCleanup, txnIntents...)
		}
	}

	// It's possible that the ABORTED state is a false negative, where the
	// transaction was in fact committed but the txn record has been removed after
	// resolving all intents (see batcheval.SynthesizeTxnFromMeta and
	// Replica.CanCreateTxnRecord). If this replica has not applied the intent
	// resolution yet, we may prematurely emit an MVCCAbortTxnOp and advance
	// the resolved ts before emitting the committed intents. This violates the
	// rangefeed checkpoint guarantee, and will at the time of writing cause the
	// changefeed to drop these events entirely. See:
	// https://github.com/cockroachdb/cockroach/issues/104309
	//
	// PushTxns will let us know if it found such an ambiguous abort. To guarantee
	// that we've applied all resolved intents in this case, submit a Barrier
	// command to the leaseholder and wait for it to apply on the local replica.
	//
	// By the time the local replica applies the barrier it will have enqueued the
	// resolved intents in the rangefeed processor's queue. These updates may not
	// yet have been applied to the resolved timestamp intent tracker, but that's
	// ok -- our MVCCAbortTxnOp will be enqueued and processed after them.
	//
	// This incurs an additional Raft write, but so would PushTxns() if we hadn't
	// hit the ambiguous abort case. This will also block until ongoing writes
	// have completed and applied, but that's fine since we currently run on our
	// own goroutine (as opposed to on a rangefeed scheduler goroutine).
	//
	// NB: We can't try to reduce the span of the barrier, because LockSpans may
	// not have the full set of intents.
	//
	// NB: PushTxnResponse.AmbiguousAbort and BarrierResponse.LeaseAppliedIndex
	// are not guaranteed to be populated prior to 24.1. In that case, we degrade
	// to the old (buggy) behavior.
	if anyAmbiguousAbort && PushTxnsBarrierEnabled.Get(&a.st.SV) {
		// The barrier will error out if our context is cancelled (which happens on
		// processor shutdown) or if the replica is destroyed. Regardless, use a 1
		// minute backstop to prevent getting wedged.
		//
		// TODO(erikgrinaker): consider removing this once we have some confidence
		// that it won't get wedged.
		err := timeutil.RunWithTimeout(ctx, "pushtxns barrier", time.Minute, a.pusher.Barrier)
		if err != nil {
			return err
		}
	}

	// Inform the processor of all logical ops.
	a.p.sendEvent(ctx, event{ops: ops}, 0)

	// Resolve intents, if necessary.
	return a.pusher.ResolveIntents(ctx, intentsToCleanup)
}

func (a *txnPushAttempt) Cancel() {
	a.done()
}

// intentsInBound returns LockUpdates for the provided transaction's LockSpans
// that intersect with the rangefeed Processor's range boundaries. For ranged
// LockSpans, a LockUpdate containing only the portion that overlaps with the
// range boundary will be returned.
//
// We filter a transaction's LockSpans to ensure that each rangefeed processor
// resolves only those intents that are within the bounds of its own range. This
// avoids unnecessary work, because a rangefeed processor only needs the intents
// in its own range to be resolved in order to advance its resolved timestamp.
// Additionally, it also avoids quadratic behavior if many rangefeed processors
// notice intents from the same transaction across many ranges. In its worst
// form, without filtering, this could create a pileup of ranged intent
// resolution across an entire table and starve out foreground traffic.
//
// NOTE: a rangefeed Processor is only configured to watch the global keyspace
// for a range. It is also only informed about logical operations on global keys
// (see OpLoggerBatch.logLogicalOp). So even if this transaction has LockSpans
// in the range's global and local keyspace, we only need to resolve those in
// the global keyspace.
func intentsInBound(txn *roachpb.Transaction, bound roachpb.Span) []roachpb.LockUpdate {
	var ret []roachpb.LockUpdate
	for _, sp := range txn.LockSpans {
		if in := sp.Intersect(bound); in.Valid() {
			ret = append(ret, roachpb.MakeLockUpdate(txn, in))
		}
	}
	return ret
}
