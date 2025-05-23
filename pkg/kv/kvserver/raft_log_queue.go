// Copyright 2015 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package kvserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvserverbase"
	"github.com/cockroachdb/cockroach/pkg/raft"
	"github.com/cockroachdb/cockroach/pkg/raft/raftpb"
	"github.com/cockroachdb/cockroach/pkg/raft/tracker"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/spanconfig"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/envutil"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/humanizeutil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
)

// Overview of Raft log truncation:
//
// The safety requirement for truncation is that the entries being truncated
// are already durably applied to the state machine. This is because after a
// truncation, the only remaining source of information regarding the data in
// the truncated entries is the state machine, which represents a prefix of
// the log. If we truncated entries that were not durably applied to the state
// machine, a crash would create a gap in what the state machine knows and the
// first entry in the untruncated log, which prevents any more application.
//
// Initialized replicas may need to provide log entries to slow followers to
// catch up, so for performance reasons they should also base truncation on
// the state of followers. Additionally, truncation should typically do work
// when there are "significant" bytes or number of entries to truncate.
// However, if the replica is quiescent we would like to truncate the whole
// log when it becomes possible.
//
// An attempt is made to add a replica to the queue under two situations:
// - Event occurs that indicates that there are significant bytes/entries that
//   can be truncated. Until the truncation is proposed (see below), these
//   events can keep firing. The queue dedups the additions until the replica
//   has been processed. Note that there is insufficient information at the
//   time of addition to predict that the truncation will actually happen.
//   Only the processing will finally decide whether truncation should happen,
//   hence the deduping cannot happen outside the queue (say by changing the
//   firing condition). If nothing is done when processing the replica, the
//   continued firing of the events will cause the replica to again be added
//   to the queue. In the current code, these events can only trigger after
//   application to the state machine.
//
// - Periodic addition via the replicaScanner: this is helpful in two ways (a)
//   the events in the previous bullet can under-fire if the size estimates
//   are wrong, (b) if the replica becomes quiescent, those events can stop
//   firing but truncation may not have been done due to other constraints
//   (like lagging followers). The periodic addition (polling) takes care of
//   ensuring that when those other constraints are removed, the truncation
//   happens.
//
// The raftLogQueue proposes "replicated" truncation. This is done by the raft
// leader, which has knowledge of the followers and results in a
// TruncateLogRequest. This proposal will be raft replicated and serve as an
// upper bound to all replicas on what can be truncated. Each replica
// remembers in-memory what truncations have been proposed, so that truncation
// can be done independently at each replica when the corresponding
// RaftAppliedIndex is durable (see raftLogTruncator). Note that since raft
// state (including truncated state) is not part of the state machine, this
// loose coupling is fine. The loose coupling is enabled with cluster version
// LooselyCoupledRaftLogTruncation and cluster setting
// kv.raft_log.enable_loosely_coupled_truncation. When not doing loose
// coupling (legacy), the proposal causes immediate truncation -- this is
// correct because other externally maintained invariants ensure that the
// state machine is durable.
//
// NB: Loosely coupled truncation loses the pending truncations that were
// queued in-memory when a node restarts. This is considered ok for now since
// either (a) the range will keep seeing new writes and eventually another
// truncation will be proposed, (b) if the range becomes quiescent we are
// willing to accept some amount of garbage. (b) can be addressed by
// unilaterally truncating at each follower if the range is quiescent. And
// since we check that the RaftAppliedIndex is durable, it is easy to truncate
// all the entries of the log in this quiescent case.

// This is a temporary cluster setting that we will remove after one release
// cycle of everyone running with the default value of true. It only exists as
// a safety switch in case the new behavior causes unanticipated issues.
// Current plan:
//   - v22.1: Has the setting. Expectation is that no one changes to false.
//   - v22.2: The code behavior is hard-coded to true, in that the setting has
//     no effect (we can also delete a bunch of legacy code).
//
// Mixed version clusters:
//   - v21.2 and v22.1: Will behave as strongly coupled since the cluster
//     version serves as an additional gate.
//   - v22.1 and v22.2: If the setting has been changed to false the v22.1 nodes
//     will do strongly coupled truncation and the v22.2 will do loosely
//     coupled. This co-existence is correct.
//
// NB: The above comment is incorrect about the default value being true. Due
// to https://github.com/cockroachdb/cockroach/issues/78412 we have changed
// the default to false for v22.1.
// TODO(sumeer): update the above comment when we have a revised plan.
var looselyCoupledTruncationEnabled = settings.RegisterBoolSetting(
	settings.SystemOnly,
	"kv.raft_log.loosely_coupled_truncation.enabled",
	"set to true to loosely couple the raft log truncation",
	false,
	settings.WithVisibility(settings.Reserved),
)

const (
	// raftLogQueueTimerDuration is the duration between truncations.
	raftLogQueueTimerDuration = 0 // zero duration to process truncations greedily
	// RaftLogQueueStaleThreshold is the minimum threshold for stale raft log
	// entries. A stale entry is one which all replicas of the range have
	// progressed past and thus is no longer needed and can be truncated.
	RaftLogQueueStaleThreshold = 100
	// RaftLogQueueStaleSize is the minimum size of the Raft log that we'll
	// truncate even if there are fewer than RaftLogQueueStaleThreshold entries
	// to truncate. The value of 64 KB was chosen experimentally by looking at
	// when Raft log truncation usually occurs when using the number of entries
	// as the sole criteria.
	RaftLogQueueStaleSize = 64 << 10
)

// raftLogQueueConcurrency limits the number of Raft log truncations to be
// processed concurrently. For a single Raft participant (range), it impacts the
// latency between consecutive log truncations, and therefore the amount of Raft
// log data flushed to disk when it wasn't truncated in memtable timely. Higher
// concurrency may decrease truncation latency and reduce the amount of IO.
var raftLogQueueConcurrency = envutil.EnvOrDefaultInt("COCKROACH_RAFT_LOG_QUEUE_CONCURRENCY", 16)

// raftLogQueue manages a queue of replicas slated to have their raft logs
// truncated by removing unneeded entries.
type raftLogQueue struct {
	*baseQueue
	db *kv.DB

	logSnapshots util.EveryN
}

var _ queueImpl = &raftLogQueue{}

// newRaftLogQueue returns a new instance of raftLogQueue. Replicas are passed
// to the queue both proactively (triggered by write load) and periodically
// (via the scanner). When processing a replica, the queue decides whether the
// Raft log can be truncated, which is a tradeoff between wanting to keep the
// log short overall and allowing slower followers to catch up before they get
// cut off by a truncation and need a snapshot. See newTruncateDecision for
// details on this decision making process.
func newRaftLogQueue(store *Store, db *kv.DB) *raftLogQueue {
	rlq := &raftLogQueue{
		db:           db,
		logSnapshots: util.Every(10 * time.Second),
	}
	rlq.baseQueue = newBaseQueue(
		"raftlog", rlq, store,
		queueConfig{
			maxSize:              defaultQueueMaxSize,
			maxConcurrency:       raftLogQueueConcurrency,
			needsLease:           false,
			needsSpanConfigs:     false,
			acceptsUnsplitRanges: true,
			successes:            store.metrics.RaftLogQueueSuccesses,
			failures:             store.metrics.RaftLogQueueFailures,
			pending:              store.metrics.RaftLogQueuePending,
			processingNanos:      store.metrics.RaftLogQueueProcessingNanos,
			disabledConfig:       kvserverbase.RaftLogQueueEnabled,
		},
	)
	return rlq
}

// newTruncateDecision returns a truncateDecision for the given Replica if no
// error occurs. If input data to establish a truncateDecision is missing, a
// zero decision is returned. When there are pending truncations queued below
// raft (see raftLogTruncator), this function pretends as if those truncations
// have already happened, and decides whether another truncation is merited.
//
// At a high level, a truncate decision operates based on the Raft log size, the
// number of entries in the log, and the Raft status of the followers. In an
// ideal world and most of the time, followers are reasonably up to date, and a
// decision to truncate to the index acked on all replicas will be made whenever
// there is at least a little bit of log to truncate (think a hundred records or
// ~100kb of data). If followers fall behind, are offline, or are waiting for a
// snapshot, a second strategy is needed to make sure that the Raft log is
// eventually truncated: when the raft log size exceeds a limit, truncations
// become willing and able to cut off followers as long as a quorum has acked
// the truncation index. The quota pool ensures that the delta between "acked by
// quorum" and "acked by all" is bounded, while Raft limits the size of the
// uncommitted, i.e. not "acked by quorum", part of the log; thus the "quorum"
// truncation strategy bounds the absolute size of the log on all followers.
//
// Exceptions are made for replicas for which information is missing ("probing
// state") as long as they are known to have been online recently, and for
// in-flight snapshots which are not adequately reflected in the Raft status and
// would otherwise be cut off with regularity. Probing live followers should
// only remain in this state for a short moment and so we deny a log truncation
// outright (as there's no safe index to truncate to); for snapshots, we can
// still truncate, but not past the snapshot's index.
//
// A challenge for log truncation is to deal with sideloaded log entries, that
// is, entries which contain SSTables for direct ingestion into the storage
// engine. Such log entries are very large, and failing to account for them in
// the heuristics can trigger overly aggressive truncations.
//
// The raft log size used in the decision making process is principally updated
// in the main Raft command apply loop, and adds a Replica to this queue
// whenever the log size has increased by a non-negligible amount that would be
// worth truncating (~100kb).
//
// Unfortunately, the size tracking is not very robust as it suffers from two
// limitations at the time of writing:
//  1. it may undercount as it is in-memory and incremented only as proposals
//     are handled; that is, a freshly started node will believe its Raft log to be
//     zero-sized independent of its actual size, and
//  2. the addition and corresponding subtraction happen in very different places
//     and are difficult to keep bug-free, meaning that there is low confidence that
//     we maintain the delta in a completely accurate manner over time. One example
//     of potential errors are sideloaded proposals, for which the subtraction needs
//     to load the size of the file on-disk (i.e. supplied by the fs), whereas
//     the addition uses the in-memory representation of the file.
//
// Ideally, a Raft log that grows large for whichever reason (for instance the
// queue being stuck on another replica) wouldn't be more than a nuisance on
// nodes with sufficient disk space. Also, IMPORT/RESTORE's split/scatter
// phase interacts poorly with overly aggressive truncations and can DDOS the
// Raft snapshot queue.
func newTruncateDecision(ctx context.Context, r *Replica) (truncateDecision, error) {
	rangeID := r.RangeID
	now := timeutil.Now()

	r.mu.RLock()
	ls := r.asLogStorage()
	raftLogSize := r.pendingLogTruncations.computePostTruncLogSize(ls.shMu.size)
	// A "cooperative" truncation (i.e. one that does not cut off followers from
	// the log) takes place whenever there are more than
	// RaftLogQueueStaleThreshold entries or the log's estimated size is above
	// RaftLogQueueStaleSize bytes. This is fairly aggressive, so under normal
	// conditions, the log is very small.
	//
	// If followers start falling behind, at some point the logs still need to
	// be truncated. We do this either when the size of the log exceeds
	// RaftLogTruncationThreshold (or, in eccentric configurations, the zone's
	// RangeMaxBytes). This captures the heuristic that at some point, it's more
	// efficient to catch up via a snapshot than via applying a long tail of log
	// entries.
	targetSize := r.store.cfg.RaftLogTruncationThreshold
	if targetSize > r.mu.conf.RangeMaxBytes {
		targetSize = r.mu.conf.RangeMaxBytes
	}
	raftStatus := r.raftStatusRLocked()

	const anyRecipientStore roachpb.StoreID = 0
	_, pendingSnapshotIndex := r.getSnapshotLogTruncationConstraintsRLocked(anyRecipientStore, false /* initialOnly */)
	lastIndex := ls.shMu.last.Index
	// NB: raftLogSize above adjusts for pending truncations that have already
	// been successfully replicated via raft, but sizeTrusted does not see if
	// those pending truncations would cause a transition from trusted =>
	// !trusted. This is done since we don't want to trigger a recomputation of
	// the raft log size while we still have pending truncations. Note that as
	// soon as those pending truncations are enacted, sizeTrusted will become
	// false, and we will recompute the size -- so this cannot cause an indefinite
	// delay in recomputation.
	logSizeTrusted := ls.shMu.sizeTrusted
	compIndex := r.raftCompactedIndexRLocked()
	r.mu.RUnlock()
	compIndex = r.pendingLogTruncations.nextCompactedIndex(compIndex)

	if raftStatus == nil {
		if log.V(6) {
			log.Infof(ctx, "the raft group doesn't exist for r%d", rangeID)
		}
		return truncateDecision{}, nil
	}

	// Is this the raft leader? We only propose log truncation on the raft
	// leader which has the up to date info on followers.
	if raftStatus.RaftState != raftpb.StateLeader {
		return truncateDecision{}, nil
	}

	// For all our followers, overwrite the RecentActive field with our own
	// activity check.
	r.mu.RLock()
	log.Eventf(ctx, "raft status before lastUpdateTimes check: %+v", raftStatus.Progress)
	log.Eventf(ctx, "lastUpdateTimes: %+v", r.mu.lastUpdateTimes)
	updateRaftProgressFromActivity(
		ctx, raftStatus.Progress, r.descRLocked().Replicas().Descriptors(),
		func(replicaID roachpb.ReplicaID) bool {
			return r.mu.lastUpdateTimes.isFollowerActiveSince(replicaID, now, r.store.cfg.RangeLeaseDuration)
		},
	)
	log.Eventf(ctx, "raft status after lastUpdateTimes check: %+v", raftStatus.Progress)
	r.mu.RUnlock()

	input := truncateDecisionInput{
		RaftStatus:           *raftStatus,
		LogSize:              raftLogSize,
		MaxLogSize:           targetSize,
		LogSizeTrusted:       logSizeTrusted,
		CompIndex:            compIndex,
		LastIndex:            lastIndex,
		PendingSnapshotIndex: pendingSnapshotIndex,
	}

	decision := computeTruncateDecision(input)
	return decision, nil
}

func updateRaftProgressFromActivity(
	ctx context.Context,
	prs map[raftpb.PeerID]tracker.Progress,
	replicas []roachpb.ReplicaDescriptor,
	replicaActive func(roachpb.ReplicaID) bool,
) {
	for _, replDesc := range replicas {
		replicaID := replDesc.ReplicaID
		pr, ok := prs[raftpb.PeerID(replicaID)]
		if !ok {
			continue
		}
		pr.RecentActive = replicaActive(replicaID)
		// Override this field for safety since we don't use it. Instead, we use
		// pendingSnapshotIndex from above.
		//
		// NOTE: We don't rely on PendingSnapshot because PendingSnapshot is
		// initialized by the leader when it realizes the follower needs a snapshot,
		// and it isn't initialized with the index of the snapshot that is actually
		// sent by us (out of band), which likely is lower.
		pr.PendingSnapshot = 0
		prs[raftpb.PeerID(replicaID)] = pr
	}
}

const (
	truncatableIndexChosenViaCommitIndex     = "commit"
	truncatableIndexChosenViaFollowers       = "followers"
	truncatableIndexChosenViaProbingFollower = "probing follower"
	truncatableIndexChosenViaPendingSnap     = "pending snapshot"
	truncatableIndexChosenViaFirstIndex      = "first index"
	truncatableIndexChosenViaLastIndex       = "last index"
)

// No assumption should be made about the relationship between
// RaftStatus.Commit, CompIndex, LastIndex. This is because:
//   - In some cases they are not updated or read atomically.
//   - CompIndex is a potentially future compacted index, after the pending
//     truncations have been applied. Currently, pending truncations are being
//     proposed through raft, so one can be sure that these pending truncations
//     do not refer to entries that are not already in the log. However, this
//     situation may change in the future. In general, we should not make an
//     assumption on what is in the local raft log based solely on CompIndex,
//     and should be based on whether CompIndex < LastIndex.
type truncateDecisionInput struct {
	RaftStatus           raft.Status
	LogSize, MaxLogSize  int64
	LogSizeTrusted       bool // false when LogSize might be off
	CompIndex            kvpb.RaftIndex
	LastIndex            kvpb.RaftIndex
	PendingSnapshotIndex kvpb.RaftIndex
}

func (input truncateDecisionInput) LogTooLarge() bool {
	return input.LogSize > input.MaxLogSize
}

// truncateDecision describes a truncation decision.
// Beware: when extending this struct, be sure to adjust .String()
// so that it is guaranteed to not contain any PII or confidential
// cluster data.
type truncateDecision struct {
	Input        truncateDecisionInput
	NewCompIndex kvpb.RaftIndex // compacted index after the log truncation
	ChosenVia    string
}

func (td *truncateDecision) raftSnapshotsForIndex(compact kvpb.RaftIndex) int {
	var n int
	for _, p := range td.Input.RaftStatus.Progress {
		if p.State != tracker.StateReplicate {
			// If the follower isn't replicating, we can't trust its Match in
			// the first place. But note that this shouldn't matter in practice
			// as we already take care to not cut off these followers when
			// computing the truncate decision. See:
			_ = truncatableIndexChosenViaProbingFollower // guru ref
			continue
		}
		// When a log truncation happens at the "current log index" (i.e. the most
		// recently committed index), it is often still in flight to the followers
		// not required for quorum, and it is likely that they won't need a
		// truncation to catch up. If Match < compact, but Next > compact, appends
		// containing this index are already in flight.
		//
		// Next <= compact means there is at least one entry that is not yet in
		// flight to this follower, so truncating now would trigger a snapshot.
		if kvpb.RaftIndex(p.Next) <= compact {
			n++
		}
	}
	// If there is a pending snapshot at some index, compacting beyond this index
	// might cause a subsequent snapshot.
	if snap := td.Input.PendingSnapshotIndex; snap != 0 && snap < compact {
		n++
	}
	return n
}

func (td *truncateDecision) NumNewRaftSnapshots() int {
	return td.raftSnapshotsForIndex(td.NewCompIndex) - td.raftSnapshotsForIndex(td.Input.CompIndex)
}

// String returns a representation for the decision.
// It is guaranteed to not return PII or confidential
// information from the cluster.
func (td *truncateDecision) String() string {
	var buf strings.Builder
	_, _ = fmt.Fprintf(&buf, "should truncate: %t [", td.ShouldTruncate())
	_, _ = fmt.Fprintf(
		&buf,
		"truncate %d entries to compacted index %d (chosen via: %s)",
		td.NumTruncatableIndexes(), td.NewCompIndex, td.ChosenVia,
	)
	if td.Input.LogTooLarge() {
		_, _ = fmt.Fprintf(
			&buf,
			"; log too large (%s > %s)",
			humanizeutil.IBytes(td.Input.LogSize),
			humanizeutil.IBytes(td.Input.MaxLogSize),
		)
	}
	if n := td.NumNewRaftSnapshots(); n > 0 {
		_, _ = fmt.Fprintf(&buf, "; implies %d Raft snapshot%s", n, util.Pluralize(int64(n)))
	}
	if !td.Input.LogSizeTrusted {
		_, _ = fmt.Fprintf(&buf, "; log size untrusted")
	}
	buf.WriteRune(']')

	return buf.String()
}

func (td *truncateDecision) NumTruncatableIndexes() int {
	if td.NewCompIndex < td.Input.CompIndex {
		return 0
	}
	return int(td.NewCompIndex - td.Input.CompIndex)
}

func (td *truncateDecision) ShouldTruncate() bool {
	n := td.NumTruncatableIndexes()
	return n >= RaftLogQueueStaleThreshold ||
		(n > 0 && td.Input.LogSize >= RaftLogQueueStaleSize)
}

// ProtectAfter attempts to prevent truncation of log indices > compacted. It
// lowers the proposed compacted index to the given one if the latter is lower.
// If this change is made, the ChosenVia annotation is updated too.
func (td *truncateDecision) ProtectAfter(compacted kvpb.RaftIndex, chosenVia string) {
	if compacted < td.NewCompIndex {
		td.NewCompIndex = compacted
		td.ChosenVia = chosenVia
	}
}

// computeTruncateDecision returns the oldest index that cannot be
// truncated. If there is a behind node, we want to keep old raft logs so it
// can catch up without having to send a full snapshot. However, if a node down
// is down long enough, sending a snapshot is more efficient and we should
// truncate the log to the next behind node or the quorum committed index. We
// currently truncate when the raft log size is bigger than the range
// size.
//
// Note that when a node is behind we continue to let the raft log build up
// instead of truncating to the commit index. Consider what would happen if we
// truncated to the commit index whenever a node is behind and thus needs to be
// caught up via a snapshot. While we're generating the snapshot, sending it to
// the behind node and waiting for it to be applied we would continue to
// truncate the log. If the snapshot generation and application takes too long
// the behind node will be caught up to a point behind the current first index
// and thus require another snapshot, likely entering a never ending loop of
// snapshots. See #8629.
func computeTruncateDecision(input truncateDecisionInput) truncateDecision {
	decision := truncateDecision{Input: input}
	commitIndex := kvpb.RaftIndex(input.RaftStatus.Commit)

	// The most aggressive possible truncation deletes the entire log. Everything
	// else in this method makes the truncation less aggressive.
	decision.NewCompIndex = input.LastIndex
	decision.ChosenVia = truncatableIndexChosenViaLastIndex

	// Start by trying to truncate at the commit index. Naively, you would expect
	// LastIndex to never be smaller than the commit index, but
	// RaftStatus.Progress.Match is updated on the leader when a command is
	// proposed and in a single replica Raft group this also means that
	// RaftStatus.Commit is updated at propose time.
	//
	// TODO(pav-kv): the above is not true. The match index is updated after a
	// durable exchange with the acceptor. The commit index is updated after doing
	// so with a quorum of acceptors, and single-replica groups are no exception.
	//
	// TODO(pav-kv): source everything from raft.LogSnapshot, and there will be no
	// discrepancy between commit index and last index.
	decision.ProtectAfter(commitIndex, truncatableIndexChosenViaCommitIndex)

	for _, progress := range input.RaftStatus.Progress {
		// Snapshots are expensive, so we try our best to avoid truncating past
		// where a follower is.

		// First, we never truncate off a recently active follower, no matter how
		// large the log gets. Recently active shares the (currently 10s) constant
		// as the quota pool, so the quota pool should put a bound on how much the
		// raft log can grow due to this.
		//
		// For live followers which are being probed (i.e. the leader doesn't know
		// how far they've caught up), the Match index is too large, and so the
		// quorum index can be, too. We don't want these followers to require a
		// snapshot since they are most likely going to be caught up very soon (they
		// respond with the "right index" to the first probe or don't respond, in
		// which case they should end up as not recently active). But we also don't
		// know their index, so we can't possible make a truncation decision that
		// avoids that at this point and make the truncation a no-op.
		//
		// The scenario in which this is most relevant is during restores, where we
		// split off new ranges that rapidly receive very large log entries while
		// the Raft group is still in a state of discovery (a new leader starts
		// probing followers at its own last index). Additionally, these ranges will
		// be split many times over, resulting in a flurry of snapshots with
		// overlapping bounds that put significant stress on the Raft snapshot
		// queue.
		//
		// NB: RecentActive is populated by updateRaftProgressFromActivity().
		if progress.RecentActive {
			if progress.State == tracker.StateProbe {
				decision.ProtectAfter(input.CompIndex, truncatableIndexChosenViaProbingFollower)
			} else {
				decision.ProtectAfter(kvpb.RaftIndex(progress.Match), truncatableIndexChosenViaFollowers)
			}
			continue
		}

		// Second, if the follower has not been recently active, we don't truncate
		// it off as long as the raft log is not too large.
		if !input.LogTooLarge() {
			decision.ProtectAfter(kvpb.RaftIndex(progress.Match), truncatableIndexChosenViaFollowers)
		}

		// Otherwise, we let it truncate to the committed index.
	}

	// The pending snapshot index acts as a placeholder for a replica that is
	// about to be added to the range (or is in Raft recovery). We don't want to
	// truncate the log in a way that will require that new replica to be caught
	// up via yet another Raft snapshot.
	if snap := input.PendingSnapshotIndex; snap > 0 {
		decision.ProtectAfter(snap, truncatableIndexChosenViaPendingSnap)
	}

	// If new compacted index dropped below the original one index, make them
	// equal (resulting in a no-op).
	if decision.NewCompIndex < input.CompIndex {
		decision.NewCompIndex = input.CompIndex
		decision.ChosenVia = truncatableIndexChosenViaFirstIndex
	}

	// The existing log slice in raft.LogStorage is described by its Compacted()
	// index and LastIndex(). The log is empty if Compacted == LastIndex.
	//
	// The input.CompIndex adjusts for the pending log truncations, which allows
	// CompIndex to be greater than LastIndex and committed index (see the comment
	// with truncateDecisionInput). So all invariant checking below is gated on
	// first ensuring that the remaining log is not empty: CompIndex < LastIndex.
	//
	// If the raft log is not empty, and there are committed entries, we can
	// assert on the following invariants:
	//
	//	(0) CompIndex     <= LastIndex
	//	(1) NewCompIndex  >= CompIndex
	//	(2) NewCompIndex  <= LastIndex
	//	(3) NewCompIndex  <= CommitIndex
	//
	// The invariants assert that we are not regressing the compacted log index,
	// and not compacting beyond what can be compacted.
	//
	// TODO(pav-kv): consider removing these checks and making them test-only. We
	// just need 100% test coverage of this logic.
	logEmpty := input.CompIndex >= input.LastIndex
	noCommittedEntries := input.CompIndex >= kvpb.RaftIndex(input.RaftStatus.Commit)

	logIndexValid := logEmpty ||
		(decision.NewCompIndex >= input.CompIndex) && (decision.NewCompIndex <= input.LastIndex)
	commitIndexValid := noCommittedEntries ||
		(decision.NewCompIndex <= commitIndex)
	valid := logIndexValid && commitIndexValid
	if !valid {
		err := fmt.Sprintf("invalid truncation decision: output = %d, input: (%d, %d], commit idx = %d",
			decision.NewCompIndex, input.CompIndex, input.LastIndex, commitIndex)
		panic(err)
	}

	return decision
}

// shouldQueue determines whether a range should be queued for truncating. This
// is true only if the replica is the raft leader and if the total number of
// the range's raft log's stale entries exceeds RaftLogQueueStaleThreshold.
func (rlq *raftLogQueue) shouldQueue(
	ctx context.Context, now hlc.ClockTimestamp, r *Replica, _ spanconfig.StoreReader,
) (shouldQueue bool, priority float64) {
	decision, err := newTruncateDecision(ctx, r)
	if err != nil {
		log.Warningf(ctx, "%v", err)
		return false, 0
	}

	shouldQ, _, prio := rlq.shouldQueueImpl(ctx, decision)
	return shouldQ, prio
}

// shouldQueueImpl returns whether the given truncate decision should lead to
// a log truncation. This is either the case if the decision says so or if
// we want to recompute the log size (in which case `recomputeRaftLogSize` and
// `shouldQ` are both true and a reasonable priority is returned).
func (rlq *raftLogQueue) shouldQueueImpl(
	ctx context.Context, decision truncateDecision,
) (shouldQ bool, recomputeRaftLogSize bool, priority float64) {
	if decision.ShouldTruncate() {
		return true, !decision.Input.LogSizeTrusted, float64(decision.Input.LogSize)
	}
	if decision.Input.LogSizeTrusted ||
		decision.Input.CompIndex >= decision.Input.LastIndex {

		return false, false, 0
	}
	// We have a nonempty log (compacted index < last index) and can't vouch that
	// the bytes in the log are known. Queue the replica; processing it will
	// force a recomputation. For the priority, we have to pick one as we
	// usually use the log size which is not available here. Going half-way
	// between zero and the MaxLogSize should give a good tradeoff between
	// processing the recomputation quickly, and not starving replicas which see
	// a significant amount of write traffic until they run over and truncate
	// more aggressively than they need to.
	// NB: this happens even on followers.
	return true, true, 1.0 + float64(decision.Input.MaxLogSize)/2.0
}

// process truncates the raft log of the range if the replica is the raft
// leader and if the total number of the range's raft log's stale entries
// exceeds RaftLogQueueStaleThreshold.
func (rlq *raftLogQueue) process(
	ctx context.Context, r *Replica, _ spanconfig.StoreReader,
) (processed bool, err error) {
	decision, err := newTruncateDecision(ctx, r)
	if err != nil {
		return false, err
	}

	if _, recompute, _ := rlq.shouldQueueImpl(ctx, decision); recompute {
		log.VEventf(ctx, 2, "recomputing raft log based on decision %+v", decision)
		if size, err := r.asLogStorage().updateLogSize(ctx); err != nil {
			return false, errors.Wrap(err, "recomputing raft log size")
		} else {
			log.VEventf(ctx, 2, "recomputed raft log size to %s", humanizeutil.IBytes(size))
		}

		// Override the decision, now that an accurate log size is available.
		decision, err = newTruncateDecision(ctx, r)
		if err != nil {
			return false, err
		}
	}

	// Can and should the raft logs be truncated?
	if !decision.ShouldTruncate() {
		log.VEventf(ctx, 3, "%s", redact.Safe(decision.String()))
		return false, nil
	}

	if n := decision.NumNewRaftSnapshots(); log.V(1) || n > 0 && rlq.logSnapshots.ShouldProcess(timeutil.Now()) {
		log.Infof(ctx, "%v", redact.Safe(decision.String()))
	} else {
		log.VEventf(ctx, 1, "%v", redact.Safe(decision.String()))
	}
	b := &kv.Batch{}
	truncRequest := &kvpb.TruncateLogRequest{
		RequestHeader:      kvpb.RequestHeader{Key: r.Desc().StartKey.AsRawKey()},
		Index:              decision.NewCompIndex + 1,
		RangeID:            r.RangeID,
		ExpectedFirstIndex: decision.Input.CompIndex + 1,
	}
	b.AddRawRequest(truncRequest)
	if err := rlq.db.Run(ctx, b); err != nil {
		return false, err
	}
	r.store.metrics.RaftLogTruncated.Inc(int64(decision.NumTruncatableIndexes()))
	return true, nil
}

func (*raftLogQueue) postProcessScheduled(
	ctx context.Context, replica replicaInQueue, priority float64,
) {
}

// timer returns interval between processing successive queued truncations.
func (*raftLogQueue) timer(_ time.Duration) time.Duration {
	return raftLogQueueTimerDuration
}

// purgatoryChan returns nil.
func (*raftLogQueue) purgatoryChan() <-chan time.Time {
	return nil
}

func (*raftLogQueue) updateChan() <-chan time.Time {
	return nil
}
