// This code has been modified from its original form by The Cockroach Authors.
// All modifications are Copyright 2024 The Cockroach Authors.
//
// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package raft

import (
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/raft/raftlogger"
	pb "github.com/cockroachdb/cockroach/pkg/raft/raftpb"
)

// LogSnapshot encapsulates a point-in-time state of the raft log accessible
// outside the raft package for reads.
//
// To access it safely, the user must not mutate the underlying raft log storage
// between when the snapshot is obtained and the reads are done.
type LogSnapshot struct {
	// compacted is the compacted log index.
	compacted uint64
	// storage contains the stable log entries.
	storage LogStorage
	// unstable contains the unstable log entries.
	unstable LeadSlice
	// termCache contains a compressed entryID suffix of raftLog.
	termCache termCache
	// logger gives access to logging errors.
	logger raftlogger.Logger
}

// termCacheSize is the default max size of the termCache. It is small because
// term flips are very rare in practice.
const termCacheSize = 4

type raftLog struct {
	// storage contains all stable entries since the last snapshot.
	storage Storage

	// unstable contains all unstable entries and snapshot.
	// they will be saved into storage.
	unstable unstable

	// termCache contains a suffix of the raftLog (both stable and unstable)
	// used for term lookup.
	termCache termCache

	// committed is the highest log position that is known to be in
	// stable storage on a quorum of nodes.
	//
	// Invariant: committed does not regress.
	committed uint64
	// applying is the highest log position that the application has
	// been instructed to apply to its state machine. Some of these
	// entries may be in the process of applying and have not yet
	// reached applied.
	// Use: The field is incremented when accepting a Ready struct.
	//
	// Invariant: applied <= applying <= committed.
	// Invariant: applying does not regress.
	applying uint64
	// applied is the highest log position that the application has
	// successfully applied to its state machine.
	// Use: The field is incremented when advancing after the committed
	// entries in a Ready struct have been applied (either synchronously
	// or asynchronously).
	//
	// Invariant: applied <= committed.
	// Invariant: applied does not regress.
	applied uint64

	logger raftlogger.Logger
}

// newLog returns a raft log initialized to the state in the given storage.
func newLog(storage Storage, logger raftlogger.Logger) *raftLog {
	compacted, lastIndex := storage.Compacted(), storage.LastIndex()
	lastTerm, err := storage.Term(lastIndex)
	if err != nil {
		panic(err) // TODO(pav-kv): the storage should always cache the last term.
	}
	last := entryID{term: lastTerm, index: lastIndex}
	return &raftLog{
		storage:   storage,
		unstable:  newUnstable(last, logger),
		termCache: newTermCache(termCacheSize, last),

		// Initialize our committed and applied pointers to the time of the last
		// compaction.
		//
		// TODO(pav-kv): this is error-prone. The applied index gets corrected
		// further, in newRaft initialization sequence. This should be done as a
		// single step.
		committed: compacted,
		applying:  compacted,
		applied:   compacted,

		logger: logger,
	}
}

func (l *raftLog) String() string {
	// TODO(pav-kv): clean-up this message. It will change all the datadriven
	// tests, so do it in a contained PR.
	return fmt.Sprintf("committed=%d, applied=%d, applying=%d, unstable.offset=%d, unstable.offsetInProgress=%d, len(unstable.Entries)=%d",
		l.committed, l.applied, l.applying, l.unstable.prev.index+1, l.unstable.entryInProgress+1, len(l.unstable.entries))
}

// accTerm returns the term of the leader whose append was accepted into the log
// last. Note that a rejected append does not update accTerm, by definition.
//
// Invariant: the log is a prefix of the accTerm's leader log
// Invariant: lastEntryID().term <= accTerm <= raft.Term
//
// In steady state, accTerm == raft.Term. When someone campaigns, raft.Term
// briefly overtakes the accTerm. However, accTerm catches up as soon as we
// accept an append from the new leader.
//
// NB: the log can be partially or fully compacted. When we say "log" above, we
// logically include all the entries that were the pre-image of a snapshot, as
// well as the entries that are still physically in the log.
func (l *raftLog) accTerm() uint64 {
	return l.unstable.term
}

// maybeAppend conditionally appends the given log slice to the log, making it
// consistent with the a.term leader log up to a.lastIndex(). A prefix of this
// log slice may already be present in the log, in which case it is skipped, and
// only the missing suffix is appended.
//
// Before appending, this may truncate a suffix of the log first, from the index
// at which a newer leader's log (and the given slice) diverges from this log.
//
// Returns false if the operation can not be done: entry a.prev does not match
// the log (so this log slice is insufficient to make our log consistent with
// the leader log), the slice is out of bounds (appending it would introduce a
// gap), or a.term is outdated.
func (l *raftLog) maybeAppend(a LeadSlice) bool {
	match, ok := l.match(a)
	if !ok {
		return false
	}
	// Fast-forward the appended log slice to the last matching entry.
	// NB: a.prev.index <= match <= a.lastIndex(), so the call is safe.
	a.LogSlice = a.forward(match)

	if len(a.entries) == 0 {
		// TODO(pav-kv): remove this clause and handle it in unstable. The log slice
		// can carry a newer a.term, which should update our accTerm.
		return true
	}
	if first := a.entries[0].Index; first <= l.committed {
		l.logger.Panicf("entry %d is already committed [committed(%d)]", first, l.committed)
	}
	if !l.unstable.truncateAndAppend(a) {
		return false
	}
	l.termCache.truncateAndAppend(a.LogSlice)
	return true
}

// append adds the given log slice to the end of the log.
//
// Returns false if the operation can not be done: entry a.prev does not match
// the lastEntryID of this log, or a.term is outdated.
func (l *raftLog) append(a LeadSlice) bool {
	if l.unstable.append(a) {
		l.termCache.truncateAndAppend(a.LogSlice)
		return true
	}
	return false
}

// match finds the longest prefix of the given log slice that matches the log.
//
// Returns the index of the last matching entry, in [s.prev.index, s.lastIndex]
// interval. The next entry either mismatches, or is missing. Returns false if
// the s.prev entry doesn't match, or is missing.
//
// All the entries up to the returned index are already present in the log, and
// do not need to be rewritten. The caller can safely fast-forward the appended
// LeadSlice to this index.
func (l *raftLog) match(s LeadSlice) (uint64, bool) {
	if !l.matchTerm(s.prev) {
		return 0, false
	}

	// TODO(pav-kv): add a fast-path here using the Log Matching property of raft.
	// Check the term match at min(s.lastIndex(), l.lastIndex()) entry, and fall
	// back to conflict search only if it mismatches.
	// TODO(pav-kv): also, there should be no mismatch if s.term == l.accTerm, so
	// the fast-path can avoid this one check too.
	//
	// TODO(pav-kv): every matchTerm call in the linear scan below can fall back
	// to fetching an entry from storage. This is inefficient, we can improve it.
	// Logs that don't match at one index, don't match at all indices above. So we
	// can use binary search to find the fork.
	match := s.prev.index
	for i := range s.entries {
		id := pbEntryID(&s.entries[i])
		if l.matchTerm(id) {
			match = id.index
			continue
		}
		if id.index <= l.lastIndex() {
			// TODO(pav-kv): should simply print %+v of the id.
			l.logger.Infof("found conflict at index %d [existing term: %d, conflicting term: %d]",
				id.index, l.zeroTermOnOutOfBounds(l.term(id.index)), id.term)
		}
		return match, true
	}
	return match, true // all entries match
}

// findConflictByTerm returns a best guess on where this log ends matching
// another log, given that the only information known about the other log is the
// (index, term) of its single entry.
//
// The first returned value is the max guessIndex <= min(index, lastIndex), such
// that term(guessIndex) <= term or term(guessIndex) is not known (because this
// index is compacted).
//
// The second returned value is the term(guessIndex), or 0 if it is unknown.
//
// This function is used by a follower and leader to resolve log conflicts after
// an unsuccessful append to a follower, and ultimately restore the steady flow
// of appends.
func (l *raftLog) findConflictByTerm(index uint64, term uint64) (uint64, uint64) {
	// Entry terms in a log are monotonic. A specific entry being (index, term)
	// means term(i) <= term for all i <= index in that log. That is, we know the
	// following information about the other log:
	//
	//	[0: 0] [1: ≤term] [2: ≤term] ... [index-1: ≤term] [index: term]
	//
	// In our log, for i <= min(index, lastIndex):
	//	1. if term(i) > term, then the logs definitely mismatch at indices >= i;
	//	2. if term(i) == term, then the logs definitely match at indices <= i;
	//	3. if term(i) < term, then the logs may or may not match at indices <= i;
	//	4. if term(i) is unknown, then the logs may or may not match at <= i.
	//
	// Property 1 follows from the inverse of the Log Matching Property: if two
	// logs mismatch at a particular index, they mismatch at all higher indices.
	//
	// Property 2 follows from the Log Matching Property. Since the other log has
	// [index: term], it necessarily has all same-term entries with lower indices
	// (otherwise this prefix wouldn't match that term leader's log). So it
	// necessarily also contains the [i: term] entry of our log.
	//
	//	Their: [0: 0] ... [i-1: ≤term] [i: term] ... [index: term]
	//	  Our: [0: 0] ... [i-1: ≤term] [i: term] ...
	//
	// Property 3 stems from the fact that we don't know terms of indices < index
	// of the other log. The term at index i may or may not match ours.
	//
	// The loop below finds the highest index i for which one of 2-4 holds.
	for index = min(index, l.lastIndex()); index > 0; index-- {
		// If there is an error (likely ErrCompacted), we don't know whether it's a
		// match or not, so assume a possible match and return the index, with 0
		// term indicating an unknown term.
		if ourTerm, err := l.term(index); err != nil {
			return index, 0
		} else if ourTerm <= term {
			return index, ourTerm
		}
	}
	return 0, 0
}

// nextUnstableEnts returns all entries that are available to be written to the
// local stable log and are not already in-progress.
func (l *raftLog) nextUnstableEnts() []pb.Entry {
	return l.unstable.nextEntries()
}

// hasNextUnstableEnts returns if there are any entries that are available to be
// written to the local stable log and are not already in-progress.
func (l *raftLog) hasNextUnstableEnts() bool {
	return len(l.nextUnstableEnts()) > 0
}

// nextCommittedEnts returns all the available entries for execution.
// Entries can be committed even when the local raft instance has not durably
// appended them to the local raft log yet. If allowUnstable is true, committed
// entries from the unstable log may be returned; otherwise, only entries known
// to reside locally on stable storage will be returned.
//
// TODO(pav-kv): only used in tests. Downgrade to a test helper or remove.
func (l *raftLog) nextCommittedEnts(allowUnstable bool) (ents []pb.Entry) {
	span := l.nextCommittedSpan(allowUnstable)
	if span.Empty() {
		return nil
	}
	ents, err := l.slice(uint64(span.After), uint64(span.Last), noLimit)
	if err != nil {
		l.logger.Panicf("unexpected error when getting unapplied entries (%v)", err)
	}
	return ents
}

// nextCommittedSpan returns the span of committed entries that can be applied.
// This is a fast check without heavy raftLog.slice() in nextCommittedEnts().
func (l *raftLog) nextCommittedSpan(allowUnstable bool) pb.LogSpan {
	return pb.LogSpan{
		After: pb.Index(l.applying),
		Last:  pb.Index(l.maxAppliableIndex(allowUnstable)),
	}
}

// maxAppliableIndex returns the maximum committed index that can be applied.
// If allowUnstable is true, committed entries from the unstable log can be
// applied; otherwise, only entries known to reside locally on stable storage
// can be applied.
//
// The maxAppliableIndex never regresses, and is always >= l.applying, assuming
// allowUnstable does not change from true to false. As of today, this flag is
// configured statically.
//
// If there is a pending snapshot, maxAppliableIndex returns l.applying, i.e.
// the application of committed entries is paused until the snapshot is applied.
func (l *raftLog) maxAppliableIndex(allowUnstable bool) uint64 {
	if l.hasNextOrInProgressSnapshot() {
		// If we have a snapshot to apply, don't return any committed entries. Doing
		// so raises questions about what should be applied first.
		//
		// TODO(pav-kv): the answer to the questions is - the snapshot should be
		// applied first, and then the entries. The code must make sure that the
		// overall sequence of "apply" batches is in the increasing order of the
		// commit index.
		return l.applying
	}
	if allowUnstable {
		return l.committed
	}
	// NB: this returns >= l.applying because l.applying <= prev.index, assuming
	// that allowUnstable hasn't flipped from true to false.
	return min(l.committed, l.unstable.prev.index)
}

// nextUnstableSnapshot returns the snapshot, if present, that is available to
// be applied to the local storage and is not already in-progress.
func (l *raftLog) nextUnstableSnapshot() *pb.Snapshot {
	return l.unstable.nextSnapshot()
}

// hasNextUnstableSnapshot returns if there is a snapshot that is available to
// be applied to the local storage and is not already in-progress.
func (l *raftLog) hasNextUnstableSnapshot() bool {
	return l.unstable.nextSnapshot() != nil
}

// hasNextOrInProgressSnapshot returns if there is pending snapshot waiting for
// applying or in the process of being applied.
func (l *raftLog) hasNextOrInProgressSnapshot() bool {
	return l.unstable.snapshot != nil
}

func (l *raftLog) snapshot() (*pb.Snapshot, error) {
	if snap := l.unstable.snapshot; snap != nil {
		return snap, nil
	}
	snap, err := l.storage.Snapshot()
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

func (l *raftLog) compacted() uint64 {
	if index, ok := l.unstable.maybeCompacted(); ok {
		return index
	}
	return l.storage.Compacted()
}

func (l *raftLog) lastIndex() uint64 {
	return l.unstable.lastIndex()
}

// commitTo bumps the commit index to the given value if it is higher than the
// current commit index.
func (l *raftLog) commitTo(mark LogMark) {
	// TODO(pav-kv): it is only safe to update the commit index if our log is
	// consistent with the mark.term leader. If the mark.term leader sees the
	// mark.index entry as committed, all future leaders have it in the log. It is
	// thus safe to bump the commit index to min(mark.index, lastIndex) if our
	// accTerm >= mark.term. Do this once raftLog/unstable tracks the accTerm.

	// never decrease commit
	if l.committed < mark.Index {
		if l.lastIndex() < mark.Index {
			l.logger.Panicf("tocommit(%d) is out of range [lastIndex(%d)]. Was the raft log corrupted, truncated, or lost?", mark.Index, l.lastIndex())
		}
		l.committed = mark.Index
	}
}

func (l *raftLog) appliedTo(i uint64) {
	if l.committed < i || i < l.applied {
		l.logger.Panicf("applied(%d) is out of range [prevApplied(%d), committed(%d)]", i, l.applied, l.committed)
	}
	l.applied = i
	l.applying = max(l.applying, i)
}

func (l *raftLog) acceptApplying(i uint64) {
	if i < l.applying || i > l.committed {
		l.logger.Panicf("applying(%d) is out of range [prevApplying(%d), committed(%d)]", i, l.applying, l.committed)
	}
	l.applying = i
}

func (l *raftLog) stableTo(mark LogMark) { l.unstable.stableTo(mark) }

func (l *raftLog) stableSnapTo(i uint64) { l.unstable.stableSnapTo(i) }

// acceptUnstable indicates that the application has started persisting the
// unstable entries in storage, and that the current unstable entries are thus
// to be marked as being in-progress, to avoid returning them with future calls
// to Ready().
func (l *raftLog) acceptUnstable() { l.unstable.acceptInProgress() }

// lastEntryID returns the ID of the last entry in the log.
func (l *raftLog) lastEntryID() entryID {
	return l.unstable.lastEntryID()
}

func (l *raftLog) term(i uint64) (uint64, error) {
	return l.snap(l.storage).term(i)
}

// term returns the term of the log entry at the given index.
func (l LogSnapshot) term(index uint64) (uint64, error) {
	// Check the unstable log first, even before computing the valid index range,
	// which may need to access the storage. If we find the entry's term in the
	// unstable log, we know it was in the valid range.
	if index > l.unstable.lastIndex() {
		return 0, ErrUnavailable
	} else if index >= l.unstable.prev.index {
		return l.unstable.termAt(index), nil
	} else if index < l.compacted {
		return 0, ErrCompacted
	}

	if term, found := l.termCache.term(index); found {
		return term, nil
	}
	term, err := l.storage.Term(index)
	if err == nil {
		return term, nil
	} else if err == ErrCompacted {
		return 0, err
	} else if err == ErrUnavailable {
		// Invariant: the log is contiguous in [l.first-1, lastIndex]. Except in
		// rare cases when there is a concurrent log truncation, and ErrCompacted is
		// returned. The ErrUnavailable here means the supposedly contiguous part of
		// this interval (note that we verified the boundaries above) in storage has
		// a missing entry, and not because of being compacted. So there is a gap.
		l.logger.Panicf("gap in the log at index %d", index)
		return 0, err
	}
	panic(err) // TODO(pav-kv): return the error and handle it up the stack.
}

// entries returns a contiguous slice of log entries at indices > after, with
// the total size not exceeding maxSize. The total size can exceed maxSize if
// the first entry (at index after+1) is larger than maxSize. Returns nil if
// there are no entries at indices > after.
func (l *raftLog) entries(after uint64, maxSize entryEncodingSize) ([]pb.Entry, error) {
	if after >= l.lastIndex() {
		return nil, nil
	}
	return l.slice(after, l.lastIndex(), maxSize)
}

// allEntries returns all entries in the log. For testing only.
func (l *raftLog) allEntries() []pb.Entry {
	ents, err := l.entries(l.compacted(), noLimit)
	if err == nil {
		return ents
	}
	if err == ErrCompacted { // try again if there was a racing compaction
		return l.allEntries()
	}
	// TODO (xiangli): handle error?
	panic(err)
}

// isUpToDate determines if a log with the given last entry is more up-to-date
// by comparing the index and term of the last entries in the existing logs.
//
// If the logs have last entries with different terms, then the log with the
// later term is more up-to-date. If the logs end with the same term, then
// whichever log has the larger lastIndex is more up-to-date. If the logs are
// the same, the given log is up-to-date.
func (l *raftLog) isUpToDate(their entryID) bool {
	our := l.lastEntryID()
	return their.term > our.term || their.term == our.term && their.index >= our.index
}

func (l *raftLog) matchTerm(id entryID) bool {
	t, err := l.term(id.index)
	if err != nil {
		return false
	}
	return t == id.term
}

func (l *raftLog) restore(s snapshot) bool {
	id := s.lastEntryID()
	l.logger.Infof("log [%s] starts to restore snapshot [index: %d, term: %d]", l, id.index, id.term)
	if !l.unstable.restore(s) {
		return false
	}
	l.termCache.reset(id)
	l.committed = id.index
	return true
}

// scan visits all log entries in the (lo, hi] range, returning them via the
// given callback. The callback can be invoked multiple times, with consecutive
// sub-ranges of the requested range. Returns up to pageSize bytes worth of
// entries at a time. May return more if a single entry size exceeds the limit.
//
// The entries in (lo, hi] must exist, otherwise scan() eventually returns an
// error (possibly after passing some entries through the callback).
//
// If the callback returns an error, scan terminates and returns this error
// immediately. This can be used to stop the scan early ("break" the loop).
func (l *raftLog) scan(lo, hi uint64, pageSize entryEncodingSize, v func([]pb.Entry) error) error {
	for lo < hi {
		ents, err := l.slice(lo, hi, pageSize)
		if err != nil {
			return err
		} else if len(ents) == 0 {
			return fmt.Errorf("got 0 entries in [%d, %d)", lo, hi)
		}
		if err := v(ents); err != nil {
			return err
		}
		lo += uint64(len(ents))
	}
	return nil
}

// slice returns a prefix of the log in the (lo, hi] interval, with the total
// entries size up to maxSize. May exceed maxSize if the first entry (lo+1) is
// larger. Returns at least one entry if the interval is non-empty.
//
// The returned slice can be appended to, but the entries in it must not be
// mutated.
func (l *raftLog) slice(lo, hi uint64, maxSize entryEncodingSize) ([]pb.Entry, error) {
	return l.snap(l.storage).slice(lo, hi, maxSize)
}

// LeadSlice returns a valid log slice for a prefix of the (lo, hi] log index
// interval, with the total entries size not exceeding maxSize.
//
// Returns at least one entry if the interval contains any. The maxSize can only
// be exceeded if the first entry (lo+1) is larger.
func (l LogSnapshot) LeadSlice(lo, hi uint64, maxSize uint64) (LeadSlice, error) {
	prevTerm, err := l.term(lo)
	if err != nil {
		// The log is probably compacted at index > lo (err == ErrCompacted), or it
		// can be a custom storage error.
		return LeadSlice{}, err
	}
	ents, err := l.slice(lo, hi, entryEncodingSize(maxSize))
	if err != nil {
		return LeadSlice{}, err
	}
	return LeadSlice{
		term: l.unstable.term,
		LogSlice: LogSlice{
			prev:    entryID{term: prevTerm, index: lo},
			entries: ents,
		},
	}, nil
}

// Slice returns log entries forming a prefix of the given log span, with the
// total entries size not exceeding maxSize.
//
// Returns at least one entry if the interval contains any. The maxSize can only
// be exceeded if the first entry (span.After+1) is larger.
func (l LogSnapshot) Slice(span pb.LogSpan, maxSize uint64) ([]pb.Entry, error) {
	return l.slice(uint64(span.After), uint64(span.Last), entryEncodingSize(maxSize))
}

func (l LogSnapshot) slice(lo, hi uint64, maxSize entryEncodingSize) ([]pb.Entry, error) {
	if err := l.mustCheckOutOfBounds(lo, hi); err != nil {
		return nil, err
	} else if lo >= hi {
		return nil, nil
	}

	// Fast path: the (lo, hi] interval is fully in the unstable log.
	if lo >= l.unstable.prev.index {
		ents := limitSize(l.unstable.sub(lo, hi), maxSize)
		// NB: use the full slice expression to protect the unstable slice from
		// potential appends to the returned slice.
		return ents[:len(ents):len(ents)], nil
	}

	// Invariant: lo < cut = min(hi, l.unstable.prev.index).
	cut := min(hi, l.unstable.prev.index)
	// TODO(pav-kv): make Entries() take (lo, hi] instead of [lo, hi), for
	// consistency. All raft log slices are constructed in context of being
	// appended after a certain index, so (lo, hi] addressing makes more sense.
	ents, err := l.storage.Entries(lo+1, cut+1, uint64(maxSize))
	if err == ErrCompacted {
		return nil, err
	} else if err == ErrUnavailable {
		l.logger.Panicf("entries(%d:%d] is unavailable from storage", lo, cut)
	} else if err != nil {
		panic(err) // TODO(pav-kv): handle errors uniformly
	}
	if hi <= l.unstable.prev.index { // all (lo, hi] entries are in storage
		return ents, nil
	}
	// Invariant below: lo < cut < hi, and cut == l.unstable.prev.index.

	// Fast path to check if ents has reached the size limitation. Either the
	// returned slice is shorter than requested (which means the next entry would
	// bring it over the limit), or a single entry reaches the limit.
	if uint64(len(ents)) < cut-lo {
		return ents, nil
	}
	// Slow path computes the actual total size, so that unstable entries are cut
	// optimally before being copied to ents slice.
	size := entsSize(ents)
	if size >= maxSize {
		return ents, nil
	}

	unstable := limitSize(l.unstable.sub(cut, hi), maxSize-size)
	// Total size of unstable may exceed maxSize-size only if len(unstable) == 1.
	// If this happens, ignore this extra entry.
	if len(unstable) == 1 && size+entsSize(unstable) > maxSize {
		return ents, nil
	}
	// Otherwise, total size of unstable does not exceed maxSize-size, so total
	// size of ents+unstable does not exceed maxSize. Simply concatenate them.
	return extend(ents, unstable), nil
}

// mustCheckOutOfBounds checks that the (lo, hi] interval is within the bounds
// of this raft log: l.compacted() <= lo <= hi <= l.lastIndex().
func (l LogSnapshot) mustCheckOutOfBounds(lo, hi uint64) error {
	if lo > hi {
		l.logger.Panicf("invalid slice %d > %d", lo, hi)
	}
	if ci := l.compacted; lo < ci {
		return ErrCompacted
	} else if li := l.unstable.lastIndex(); hi > li {
		l.logger.Panicf("slice(%d,%d] out of bound (%d,%d]", lo, hi, ci, li)
	}
	return nil
}

func (l *raftLog) zeroTermOnOutOfBounds(t uint64, err error) uint64 {
	if err == nil {
		return t
	}
	if err == ErrCompacted || err == ErrUnavailable {
		return 0
	}
	l.logger.Panicf("unexpected error (%v)", err)
	return 0
}

// snap returns a point-in-time snapshot of the raft log. This snapshot can be
// read from while the underlying storage is not mutated.
func (l *raftLog) snap(storage LogStorage) LogSnapshot {
	// NB: termCache and unstable slice are safe to copy, and make sure to not
	// corrupt their shallow copies.
	return LogSnapshot{
		compacted: l.compacted(),
		storage:   storage,
		unstable:  l.unstable.LeadSlice,
		termCache: l.termCache,
		logger:    l.logger,
	}
}
