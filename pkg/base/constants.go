// Copyright 2016 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package base

import "time"

const (
	// DefaultMaxClockOffset is the default maximum acceptable clock offset value.
	// On Azure, clock offsets between 250ms and 500ms are common. On AWS and GCE,
	// clock offsets generally stay below 250ms. See comments on Config.MaxOffset
	// for more on this setting.
	DefaultMaxClockOffset = 500 * time.Millisecond

	// DefaultTxnHeartbeatInterval is how often heartbeats are sent from the
	// transaction coordinator to a live transaction. These keep it from
	// being preempted by other transactions writing the same keys. If a
	// transaction fails to be heartbeat within 5x the heartbeat interval,
	// it may be aborted by conflicting txns.
	DefaultTxnHeartbeatInterval = 1 * time.Second

	// SlowRequestThreshold is the amount of time to wait before considering a
	// request to be "slow".
	SlowRequestThreshold = 15 * time.Second

	// ChunkRaftCommandThresholdBytes is the threshold in bytes at which
	// to chunk or otherwise limit commands being sent to Raft.
	ChunkRaftCommandThresholdBytes = 256 * 1000

	// HeapProfileDir is the directory name where the heap profiler stores profiles
	// when there is a potential OOM situation.
	HeapProfileDir = "heap_profiler"

	// GoroutineDumpDir is the directory name where the goroutine dumper
	// stores dump when one of the dump heuristics is triggered.
	GoroutineDumpDir = "goroutine_dump"

	// CPUProfileDir is the directory name where the CPU profile dumper
	// stores profiles when the periodic CPU profile dump is enabled.
	CPUProfileDir = "pprof_dump"

	// ExecutionTraceDir is the directory name that holds Go execution traces
	// when the execution trace dumper is enabled.
	ExecutionTraceDir = "executiontrace_dump"

	// InflightTraceDir is the directory name where the job trace dumper stores traces
	// when a job opts in to dumping its execution traces.
	InflightTraceDir = "inflight_trace_dump"
)
