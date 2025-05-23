// Copyright 2024 PingCAP, Inc.
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

package join

import (
	"context"
	"hash"
	"math"
	"math/bits"
	"math/rand"
	"runtime/trace"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/pkg/executor/internal/exec"
	"github.com/pingcap/tidb/pkg/executor/join/joinversion"
	"github.com/pingcap/tidb/pkg/expression"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/planner/core/operator/logicalop"
	"github.com/pingcap/tidb/pkg/sessionctx/vardef"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util"
	"github.com/pingcap/tidb/pkg/util/channel"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/disk"
	"github.com/pingcap/tidb/pkg/util/intest"
	"github.com/pingcap/tidb/pkg/util/memory"
)

const minimalHashTableLen = 32

var (
	_ exec.Executor = &HashJoinV2Exec{}
	// EnableHashJoinV2 enable hash join v2, used for test
	EnableHashJoinV2 = "set tidb_hash_join_version = " + joinversion.HashJoinVersionOptimized
	// DisableHashJoinV2 disable hash join v2, used for test
	DisableHashJoinV2 = "set tidb_hash_join_version = " + joinversion.HashJoinVersionLegacy
	// HashJoinV2Strings is used for test
	HashJoinV2Strings = []string{DisableHashJoinV2, EnableHashJoinV2}
)

type hashTableContext struct {
	// rowTables is used during split partition stage, each buildWorker has
	// its own rowTable
	rowTables     [][]*rowTable
	hashTable     *hashTableV2
	tagHelper     *tagPtrHelper
	memoryTracker *memory.Tracker
}

func (htc *hashTableContext) reset() {
	htc.rowTables = nil
	htc.hashTable = nil
	htc.tagHelper = nil
	htc.memoryTracker.Detach()
}

func (htc *hashTableContext) getAllMemoryUsageInHashTable() int64 {
	partNum := len(htc.hashTable.tables)
	totalMemoryUsage := int64(0)
	for i := 0; i < partNum; i++ {
		mem := htc.hashTable.getPartitionMemoryUsage(i)
		totalMemoryUsage += mem
	}
	return totalMemoryUsage
}

func (htc *hashTableContext) clearHashTable() {
	partNum := len(htc.hashTable.tables)
	for i := 0; i < partNum; i++ {
		htc.hashTable.clearPartitionSegments(i)
	}
}

func (htc *hashTableContext) getPartitionMemoryUsage(partID int) int64 {
	totalMemoryUsage := int64(0)
	for _, tables := range htc.rowTables {
		if tables != nil && tables[partID] != nil {
			totalMemoryUsage += tables[partID].getTotalUsedBytesInSegments()
		}
	}

	return totalMemoryUsage
}

func (htc *hashTableContext) getSegmentsInRowTable(workerID, partitionID int) []*rowTableSegment {
	if htc.rowTables[workerID] != nil && htc.rowTables[workerID][partitionID] != nil {
		return htc.rowTables[workerID][partitionID].getSegments()
	}

	return nil
}

func (htc *hashTableContext) getAllSegmentsMemoryUsageInRowTable() int64 {
	totalMemoryUsage := int64(0)
	for _, tables := range htc.rowTables {
		for _, table := range tables {
			if table != nil {
				totalMemoryUsage += table.getTotalMemoryUsage()
			}
		}
	}
	return totalMemoryUsage
}

func (htc *hashTableContext) clearAllSegmentsInRowTable() {
	for _, tables := range htc.rowTables {
		for _, table := range tables {
			if table != nil {
				table.clearSegments()
			}
		}
	}
}

func (htc *hashTableContext) clearSegmentsInRowTable(workerID, partitionID int) {
	if htc.rowTables[workerID] != nil && htc.rowTables[workerID][partitionID] != nil {
		htc.rowTables[workerID][partitionID].clearSegments()
	}
}

func (htc *hashTableContext) build(task *buildTask) {
	htc.hashTable.tables[task.partitionIdx].build(task.segStartIdx, task.segEndIdx, htc.tagHelper)
}

func (htc *hashTableContext) lookup(partitionIndex int, hashValue uint64) taggedPtr {
	return htc.hashTable.tables[partitionIndex].lookup(hashValue, htc.tagHelper)
}

func (htc *hashTableContext) getCurrentRowSegment(workerID, partitionID int, allowCreate bool, firstSegSizeHint uint) *rowTableSegment {
	if htc.rowTables[workerID][partitionID] == nil {
		htc.rowTables[workerID][partitionID] = newRowTable()
	}
	segNum := len(htc.rowTables[workerID][partitionID].segments)
	if segNum == 0 || htc.rowTables[workerID][partitionID].segments[segNum-1].finalized {
		if !allowCreate {
			panic("logical error, should not reach here")
		}
		// do not pre-allocate too many memory for the first seg because for query that only has a few rows, it may waste memory and may hurt the performance in high concurrency scenarios
		rowSizeHint := maxRowTableSegmentSize
		if segNum == 0 {
			rowSizeHint = int64(firstSegSizeHint)
		}
		seg := newRowTableSegment(uint(rowSizeHint))
		htc.rowTables[workerID][partitionID].segments = append(htc.rowTables[workerID][partitionID].segments, seg)
		segNum++
	}
	return htc.rowTables[workerID][partitionID].segments[segNum-1]
}

func (htc *hashTableContext) finalizeCurrentSeg(workerID, partitionID int, builder *rowTableBuilder, needConsume bool) {
	seg := htc.getCurrentRowSegment(workerID, partitionID, false, 0)
	builder.rowNumberInCurrentRowTableSeg[partitionID] = 0
	failpoint.Inject("finalizeCurrentSegPanic", nil)
	seg.initTaggedBits()
	seg.finalized = true
	if needConsume {
		htc.memoryTracker.Consume(seg.totalUsedBytes())
	}
}

func (*hashTableContext) calculateHashTableMemoryUsage(rowTables []*rowTable) (int64, []int64) {
	totalMemoryUsage := int64(0)
	partitionsMemoryUsage := make([]int64, 0)
	for _, table := range rowTables {
		hashTableLength := getHashTableLengthByRowTable(table)
		memoryUsage := getHashTableMemoryUsage(hashTableLength)
		partitionsMemoryUsage = append(partitionsMemoryUsage, memoryUsage)
		totalMemoryUsage += memoryUsage
	}
	return totalMemoryUsage, partitionsMemoryUsage
}

// In order to avoid the allocation of hash table, we pre-calculate the memory usage in advance
// to know which hash tables need to be created.
func (htc *hashTableContext) tryToSpill(rowTables []*rowTable, spillHelper *hashJoinSpillHelper) ([]*rowTable, error) {
	totalMemoryUsage, hashTableMemoryUsage := htc.calculateHashTableMemoryUsage(rowTables)

	// Pre-consume the memory usage
	htc.memoryTracker.Consume(totalMemoryUsage)

	if spillHelper != nil && spillHelper.isSpillNeeded() {
		spillHelper.spillTriggeredBeforeBuildingHashTableForTest = true
		err := spillHelper.spillRowTable(hashTableMemoryUsage)
		if err != nil {
			return nil, err
		}

		spilledPartition := spillHelper.getSpilledPartitions()
		for _, partID := range spilledPartition {
			// Clear spilled row tables
			rowTables[partID].clearSegments()
		}

		// Though some partitions have been spilled or are empty, their hash tables are still be created
		// because probe rows in these partitions may access their hash tables.
		// We need to consider these memory usage.
		totalDefaultMemUsage := getHashTableMemoryUsage(minimalHashTableLen) * int64(len(spilledPartition))

		// Hash table memory usage has already been released in spill operation.
		// So it's unnecessary to release them again.
		htc.memoryTracker.Consume(totalDefaultMemUsage)
	}

	return rowTables, nil
}

func (htc *hashTableContext) mergeRowTablesToHashTable(partitionNumber uint, spillHelper *hashJoinSpillHelper) (int, error) {
	rowTables := make([]*rowTable, partitionNumber)
	for i := 0; i < int(partitionNumber); i++ {
		rowTables[i] = newRowTable()
	}

	totalSegmentCnt := 0
	for _, rowTablesPerWorker := range htc.rowTables {
		for partIdx, rt := range rowTablesPerWorker {
			if rt == nil {
				continue
			}
			rowTables[partIdx].merge(rt)
			totalSegmentCnt += len(rt.segments)
		}
	}

	var err error

	// spillHelper may be nil in ut
	if spillHelper != nil {
		rowTables, err = htc.tryToSpill(rowTables, spillHelper)
		if err != nil {
			return 0, err
		}

		spillHelper.setCanSpillFlag(false)
	}

	taggedBits := uint8(maxTaggedBits)
	for i := 0; i < int(partitionNumber); i++ {
		for _, seg := range rowTables[i].segments {
			taggedBits = min(taggedBits, seg.taggedBits)
		}
		htc.hashTable.tables[i] = newSubTable(rowTables[i])
	}

	htc.tagHelper = &tagPtrHelper{}
	htc.tagHelper.init(taggedBits)

	htc.clearAllSegmentsInRowTable()
	return totalSegmentCnt, nil
}

// HashJoinCtxV2 is the hash join ctx used in hash join v2
type HashJoinCtxV2 struct {
	hashJoinCtxBase
	partitionNumber     uint
	partitionMaskOffset int
	ProbeKeyTypes       []*types.FieldType
	BuildKeyTypes       []*types.FieldType
	stats               *hashJoinRuntimeStatsV2

	RightAsBuildSide               bool
	BuildFilter                    expression.CNFExprs
	ProbeFilter                    expression.CNFExprs
	OtherCondition                 expression.CNFExprs
	hashTableContext               *hashTableContext
	hashTableMeta                  *joinTableMeta
	needScanRowTableAfterProbeDone bool

	LUsed, RUsed                                 []int
	LUsedInOtherCondition, RUsedInOtherCondition []int

	maxSpillRound int
	spillHelper   *hashJoinSpillHelper
	spillAction   *hashJoinSpillAction
}

func (hCtx *HashJoinCtxV2) resetHashTableContextForRestore() {
	memoryUsage := hCtx.hashTableContext.getAllSegmentsMemoryUsageInRowTable()
	hCtx.hashTableContext.clearAllSegmentsInRowTable()
	hCtx.hashTableContext.memoryTracker.Consume(-memoryUsage)

	memoryUsage = hCtx.hashTableContext.getAllMemoryUsageInHashTable()
	hCtx.hashTableContext.clearHashTable()
	hCtx.hashTableContext.memoryTracker.Consume(-memoryUsage)
}

// partitionNumber is always power of 2
func genHashJoinPartitionNumber(partitionHint uint) uint {
	partitionNumber := uint(1)
	for partitionNumber < partitionHint && partitionNumber < 16 {
		partitionNumber <<= 1
	}
	return partitionNumber
}

func getPartitionMaskOffset(partitionNumber uint) int {
	msbPos := bits.TrailingZeros64(uint64(partitionNumber))
	// top MSB bits in hash value will be used to partition data
	return 64 - msbPos
}

// SetupPartitionInfo set up partitionNumber and partitionMaskOffset based on concurrency
func (hCtx *HashJoinCtxV2) SetupPartitionInfo() {
	hCtx.partitionNumber = genHashJoinPartitionNumber(hCtx.Concurrency)
	hCtx.partitionMaskOffset = getPartitionMaskOffset(hCtx.partitionNumber)
}

// initHashTableContext create hashTableContext for current HashJoinCtxV2
func (hCtx *HashJoinCtxV2) initHashTableContext() {
	hCtx.hashTableContext = &hashTableContext{}
	hCtx.hashTableContext.rowTables = make([][]*rowTable, hCtx.Concurrency)
	for index := range hCtx.hashTableContext.rowTables {
		hCtx.hashTableContext.rowTables[index] = make([]*rowTable, hCtx.partitionNumber)
	}
	hCtx.hashTableContext.hashTable = &hashTableV2{
		tables:          make([]*subTable, hCtx.partitionNumber),
		partitionNumber: uint64(hCtx.partitionNumber),
	}
	hCtx.hashTableContext.memoryTracker = memory.NewTracker(memory.LabelForHashTableInHashJoinV2, -1)
}

// ProbeSideTupleFetcherV2 reads tuples from ProbeSideExec and send them to ProbeWorkers.
type ProbeSideTupleFetcherV2 struct {
	probeSideTupleFetcherBase
	*HashJoinCtxV2
	canSkipProbeIfHashTableIsEmpty bool
}

// ProbeWorkerV2 is the probe worker used in hash join v2
type ProbeWorkerV2 struct {
	probeWorkerBase
	HashJoinCtx *HashJoinCtxV2
	// We build individual joinProbe for each join worker when use chunk-based
	// execution, to avoid the concurrency of joiner.chk and joiner.selected.
	JoinProbe ProbeV2

	restoredChkBuf *chunk.Chunk
}

func (w *ProbeWorkerV2) updateProbeStatistic(start time.Time, probeTime int64) {
	t := time.Since(start)
	atomic.AddInt64(&w.HashJoinCtx.stats.probe, probeTime)
	atomic.AddInt64(&w.HashJoinCtx.stats.workerFetchAndProbe, int64(t))
	setMaxValue(&w.HashJoinCtx.stats.maxProbeForCurrentRound, probeTime)
	setMaxValue(&w.HashJoinCtx.stats.maxWorkerFetchAndProbeForCurrentRound, int64(t))
}

func (w *ProbeWorkerV2) restoreAndProbe(inDisk *chunk.DataInDiskByChunks, start time.Time) {
	probeTime := int64(0)
	if w.HashJoinCtx.stats != nil {
		defer func() {
			w.updateProbeStatistic(start, probeTime)
		}()
	}

	ok, joinResult := w.getNewJoinResult()
	if !ok {
		return
	}

	chunkNum := inDisk.NumChunks()

	for i := 0; i < chunkNum; i++ {
		select {
		case <-w.HashJoinCtx.closeCh:
			return
		default:
		}
		failpoint.Inject("ConsumeRandomPanic", nil)

		err := inDisk.FillChunk(i, w.restoredChkBuf)
		if err != nil {
			joinResult.err = err
			break
		}

		err = triggerIntest(2)
		if err != nil {
			joinResult.err = err
			break
		}

		start := time.Now()
		waitTime := int64(0)
		ok, waitTime, joinResult = w.processOneRestoredProbeChunk(joinResult)
		probeTime += int64(time.Since(start)) - waitTime
		if !ok {
			break
		}
	}

	err := w.JoinProbe.SpillRemainingProbeChunks()
	if err != nil {
		joinResult.err = err
	}

	if joinResult.err != nil || (joinResult.chk != nil && joinResult.chk.NumRows() > 0) {
		w.HashJoinCtx.joinResultCh <- joinResult
	} else if joinResult.chk != nil && joinResult.chk.NumRows() == 0 {
		w.joinChkResourceCh <- joinResult.chk
	}
}

// BuildWorkerV2 is the build worker used in hash join v2
type BuildWorkerV2 struct {
	buildWorkerBase
	HashJoinCtx    *HashJoinCtxV2
	BuildTypes     []*types.FieldType
	HasNullableKey bool
	WorkerID       uint
	builder        *rowTableBuilder
	restoredChkBuf *chunk.Chunk
}

func (b *BuildWorkerV2) getSegmentsInRowTable(partID int) []*rowTableSegment {
	return b.HashJoinCtx.hashTableContext.getSegmentsInRowTable(int(b.WorkerID), partID)
}

func (b *BuildWorkerV2) clearSegmentsInRowTable(partID int) {
	b.HashJoinCtx.hashTableContext.clearSegmentsInRowTable(int(b.WorkerID), partID)
}

func (b *BuildWorkerV2) updatePartitionData(cost int64) {
	atomic.AddInt64(&b.HashJoinCtx.stats.partitionData, cost)
	setMaxValue(&b.HashJoinCtx.stats.maxPartitionDataForCurrentRound, cost)
}

func (b *BuildWorkerV2) processOneRestoredChunk(cost *int64) error {
	start := time.Now()
	err := b.builder.processOneRestoredChunk(b.restoredChkBuf, b.HashJoinCtx, int(b.WorkerID), int(b.HashJoinCtx.partitionNumber))
	if err != nil {
		return err
	}
	*cost += int64(time.Since(start))
	return nil
}

func (b *BuildWorkerV2) splitPartitionAndAppendToRowTableForRestoreImpl(i int, inDisk *chunk.DataInDiskByChunks, fetcherAndWorkerSyncer *sync.WaitGroup, hasErr bool, cost *int64) (err error) {
	defer func() {
		fetcherAndWorkerSyncer.Done()

		if r := recover(); r != nil {
			// We shouldn't throw the panic out of this function, or
			// we can't continue to consume `syncCh` channel and call
			// the `Done` function of `fetcherAndWorkerSyncer`.
			// So it's necessary to handle it here.
			err = util.GetRecoverError(r)
		}
	}()

	if hasErr {
		return nil
	}

	err = inDisk.FillChunk(i, b.restoredChkBuf)
	if err != nil {
		return err
	}

	err = triggerIntest(3)
	if err != nil {
		return err
	}

	err = b.processOneRestoredChunk(cost)
	if err != nil {
		return err
	}
	return nil
}

func (b *BuildWorkerV2) splitPartitionAndAppendToRowTableForRestore(inDisk *chunk.DataInDiskByChunks, syncCh chan *chunk.Chunk, waitForController chan struct{}, fetcherAndWorkerSyncer *sync.WaitGroup, errCh chan error, doneCh chan struct{}) {
	cost := int64(0)
	defer func() {
		if b.HashJoinCtx.stats != nil {
			b.updatePartitionData(cost)
		}
	}()

	hasErr := false
	chunkNum := inDisk.NumChunks()
	for i := 0; i < chunkNum; i++ {
		_, ok := <-syncCh
		if !ok {
			break
		}

		err := b.splitPartitionAndAppendToRowTableForRestoreImpl(i, inDisk, fetcherAndWorkerSyncer, hasErr, &cost)
		if err != nil {
			hasErr = true
			handleErr(err, errCh, doneCh)
		}
	}

	// Wait for command from the controller so that we can avoid data race with the spill executed in controller
	<-waitForController

	if hasErr {
		return
	}

	start := time.Now()
	b.builder.appendRemainingRowLocations(int(b.WorkerID), b.HashJoinCtx.hashTableContext)
	cost += int64(time.Since(start))
}

func (b *BuildWorkerV2) splitPartitionAndAppendToRowTable(typeCtx types.Context, fetcherAndWorkerSyncer *sync.WaitGroup, srcChkCh chan *chunk.Chunk, errCh chan error, doneCh chan struct{}) {
	cost := int64(0)
	defer func() {
		if b.HashJoinCtx.stats != nil {
			b.updatePartitionData(cost)
		}
	}()

	hasErr := false
	for chk := range srcChkCh {
		err := b.splitPartitionAndAppendToRowTableImpl(typeCtx, chk, fetcherAndWorkerSyncer, hasErr, &cost)
		if err != nil {
			hasErr = true
			handleErr(err, errCh, doneCh)
		}
	}

	if hasErr {
		return
	}

	start := time.Now()
	b.builder.appendRemainingRowLocations(int(b.WorkerID), b.HashJoinCtx.hashTableContext)
	cost += int64(time.Since(start))
}

func (b *BuildWorkerV2) processOneChunk(typeCtx types.Context, chk *chunk.Chunk, cost *int64) error {
	start := time.Now()
	err := b.builder.processOneChunk(chk, typeCtx, b.HashJoinCtx, int(b.WorkerID))
	failpoint.Inject("splitPartitionPanic", nil)
	*cost += int64(time.Since(start))
	return err
}

func (b *BuildWorkerV2) splitPartitionAndAppendToRowTableImpl(typeCtx types.Context, chk *chunk.Chunk, fetcherAndWorkerSyncer *sync.WaitGroup, hasErr bool, cost *int64) error {
	defer func() {
		fetcherAndWorkerSyncer.Done()
	}()

	if hasErr {
		return nil
	}

	err := triggerIntest(5)
	if err != nil {
		return err
	}

	err = b.processOneChunk(typeCtx, chk, cost)
	if err != nil {
		return err
	}
	return nil
}

// buildHashTableForList builds hash table from `list`.
func (b *BuildWorkerV2) buildHashTable(taskCh chan *buildTask) error {
	cost := int64(0)
	defer func() {
		if b.HashJoinCtx.stats != nil {
			atomic.AddInt64(&b.HashJoinCtx.stats.buildHashTable, cost)
			setMaxValue(&b.HashJoinCtx.stats.maxBuildHashTableForCurrentRound, cost)
		}
	}()
	for task := range taskCh {
		start := time.Now()
		b.HashJoinCtx.hashTableContext.build(task)
		failpoint.Inject("buildHashTablePanic", nil)
		cost += int64(time.Since(start))
		err := triggerIntest(5)
		if err != nil {
			return err
		}
	}
	return nil
}

// NewJoinBuildWorkerV2 create a BuildWorkerV2
func NewJoinBuildWorkerV2(ctx *HashJoinCtxV2, workID uint, buildSideExec exec.Executor, buildKeyColIdx []int, buildTypes []*types.FieldType) *BuildWorkerV2 {
	hasNullableKey := false
	for _, idx := range buildKeyColIdx {
		if !mysql.HasNotNullFlag(buildTypes[idx].GetFlag()) {
			hasNullableKey = true
			break
		}
	}
	worker := &BuildWorkerV2{
		HashJoinCtx:    ctx,
		BuildTypes:     buildTypes,
		WorkerID:       workID,
		HasNullableKey: hasNullableKey,
	}
	worker.BuildSideExec = buildSideExec
	worker.BuildKeyColIdx = buildKeyColIdx
	return worker
}

// HashJoinV2Exec implements the hash join algorithm.
type HashJoinV2Exec struct {
	exec.BaseExecutor
	*HashJoinCtxV2

	ProbeSideTupleFetcher *ProbeSideTupleFetcherV2
	ProbeWorkers          []*ProbeWorkerV2
	BuildWorkers          []*BuildWorkerV2

	workerWg util.WaitGroupWrapper
	waiterWg util.WaitGroupWrapper

	restoredBuildInDisk []*chunk.DataInDiskByChunks
	restoredProbeInDisk []*chunk.DataInDiskByChunks

	prepared  bool
	inRestore bool

	IsGA bool

	isMemoryClearedForTest bool
}

func (e *HashJoinV2Exec) isAllMemoryClearedForTest() bool {
	return e.isMemoryClearedForTest
}

func (e *HashJoinV2Exec) initMaxSpillRound() {
	if e.partitionNumber > 1024 {
		e.maxSpillRound = 1
		return
	}

	// Calculate the minimum number of rounds required for the total partitions to exceed 1024
	e.maxSpillRound = int(math.Log(1024) / math.Log(float64(e.partitionNumber)))
}

// Close implements the Executor Close interface.
func (e *HashJoinV2Exec) Close() error {
	if e.closeCh != nil {
		close(e.closeCh)
	}
	e.finished.Store(true)
	if e.prepared {
		if e.buildFinished != nil {
			channel.Clear(e.buildFinished)
		}
		if e.joinResultCh != nil {
			channel.Clear(e.joinResultCh)
		}
		if e.ProbeSideTupleFetcher.probeChkResourceCh != nil {
			close(e.ProbeSideTupleFetcher.probeChkResourceCh)
			channel.Clear(e.ProbeSideTupleFetcher.probeChkResourceCh)
		}
		for i := range e.ProbeSideTupleFetcher.probeResultChs {
			channel.Clear(e.ProbeSideTupleFetcher.probeResultChs[i])
		}
		for i := range e.ProbeWorkers {
			close(e.ProbeWorkers[i].joinChkResourceCh)
			channel.Clear(e.ProbeWorkers[i].joinChkResourceCh)
		}
		e.ProbeSideTupleFetcher.probeChkResourceCh = nil
		e.waiterWg.Wait()
		e.hashTableContext.reset()
	}
	for _, w := range e.ProbeWorkers {
		w.joinChkResourceCh = nil
	}

	if e.stats != nil {
		defer e.Ctx().GetSessionVars().StmtCtx.RuntimeStatsColl.RegisterStats(e.ID(), e.stats)
	}
	e.releaseDisk()
	if e.spillHelper != nil {
		e.spillHelper.close()
	}
	err := e.BaseExecutor.Close()
	return err
}

// Open implements the Executor Open interface.
func (e *HashJoinV2Exec) Open(ctx context.Context) error {
	if err := e.BaseExecutor.Open(ctx); err != nil {
		e.closeCh = nil
		e.prepared = false
		return err
	}
	return e.OpenSelf()
}

// OpenSelf opens hash join itself and initializes the hash join context.
func (e *HashJoinV2Exec) OpenSelf() error {
	e.prepared = false
	e.inRestore = false
	needScanRowTableAfterProbeDone := e.ProbeWorkers[0].JoinProbe.NeedScanRowTable()
	e.HashJoinCtxV2.needScanRowTableAfterProbeDone = needScanRowTableAfterProbeDone
	if e.RightAsBuildSide {
		e.hashTableMeta = newTableMeta(e.BuildWorkers[0].BuildKeyColIdx, e.BuildWorkers[0].BuildTypes,
			e.BuildKeyTypes, e.ProbeKeyTypes, e.RUsedInOtherCondition, e.RUsed, needScanRowTableAfterProbeDone)
	} else {
		e.hashTableMeta = newTableMeta(e.BuildWorkers[0].BuildKeyColIdx, e.BuildWorkers[0].BuildTypes,
			e.BuildKeyTypes, e.ProbeKeyTypes, e.LUsedInOtherCondition, e.LUsed, needScanRowTableAfterProbeDone)
	}
	e.HashJoinCtxV2.ChunkAllocPool = e.AllocPool
	if e.memTracker != nil {
		e.memTracker.Reset()
	} else {
		e.memTracker = memory.NewTracker(e.ID(), -1)
	}
	e.memTracker.AttachTo(e.Ctx().GetSessionVars().StmtCtx.MemTracker)

	if e.diskTracker != nil {
		e.diskTracker.Reset()
	} else {
		e.diskTracker = disk.NewTracker(e.ID(), -1)
	}
	e.diskTracker.AttachTo(e.Ctx().GetSessionVars().StmtCtx.DiskTracker)
	e.spillHelper = newHashJoinSpillHelper(e, int(e.partitionNumber), e.ProbeSideTupleFetcher.ProbeSideExec.RetFieldTypes())
	e.maxSpillRound = 1

	if vardef.EnableTmpStorageOnOOM.Load() && e.partitionNumber > 1 {
		e.initMaxSpillRound()
		e.spillAction = newHashJoinSpillAction(e.spillHelper)
		e.Ctx().GetSessionVars().MemTracker.FallbackOldAndSetNewAction(e.spillAction)
	}

	e.workerWg = util.WaitGroupWrapper{}
	e.waiterWg = util.WaitGroupWrapper{}
	e.closeCh = make(chan struct{})
	e.finished.Store(false)

	if e.RuntimeStats() != nil && e.stats == nil {
		e.stats = &hashJoinRuntimeStatsV2{}
		e.stats.concurrent = int(e.Concurrency)
	}

	if e.stats != nil {
		e.stats.reset()
		e.stats.spill.partitionNum = int(e.partitionNumber)
		e.stats.isHashJoinGA = e.IsGA
	}
	return nil
}

func (fetcher *ProbeSideTupleFetcherV2) shouldLimitProbeFetchSize() bool {
	if fetcher.JoinType == logicalop.LeftOuterJoin && fetcher.RightAsBuildSide {
		return true
	}
	if fetcher.JoinType == logicalop.RightOuterJoin && !fetcher.RightAsBuildSide {
		return true
	}
	return false
}

func (e *HashJoinV2Exec) canSkipProbeIfHashTableIsEmpty() bool {
	switch e.JoinType {
	case logicalop.InnerJoin:
		return true
	case logicalop.LeftOuterJoin:
		return !e.RightAsBuildSide
	case logicalop.RightOuterJoin:
		return e.RightAsBuildSide
	case logicalop.SemiJoin:
		return e.RightAsBuildSide
	default:
		return false
	}
}

func (e *HashJoinV2Exec) initializeForProbe() {
	e.ProbeSideTupleFetcher.HashJoinCtxV2 = e.HashJoinCtxV2
	// e.joinResultCh is for transmitting the join result chunks to the main thread.
	e.joinResultCh = make(chan *hashjoinWorkerResult, e.Concurrency+1)
	e.ProbeSideTupleFetcher.initializeForProbeBase(e.Concurrency, e.joinResultCh)
	e.ProbeSideTupleFetcher.canSkipProbeIfHashTableIsEmpty = e.canSkipProbeIfHashTableIsEmpty()
	e.ProbeSideTupleFetcher.canSkipScanRowTable = false

	for i := uint(0); i < e.Concurrency; i++ {
		e.ProbeWorkers[i].initializeForProbe(e.ProbeSideTupleFetcher.probeChkResourceCh, e.ProbeSideTupleFetcher.probeResultChs[i], e)
		e.ProbeWorkers[i].JoinProbe.ResetProbeCollision()
	}
}

func (e *HashJoinV2Exec) startProbeFetcher(ctx context.Context) {
	if !e.inRestore {
		fetchProbeSideChunksFunc := func() {
			defer trace.StartRegion(ctx, "HashJoinProbeSideFetcher").End()
			e.ProbeSideTupleFetcher.fetchProbeSideChunks(
				ctx,
				e.MaxChunkSize(),
				func() bool { return e.ProbeSideTupleFetcher.hashTableContext.hashTable.isHashTableEmpty() },
				func() bool { return e.spillHelper.isSpillTriggered() },
				e.ProbeSideTupleFetcher.canSkipProbeIfHashTableIsEmpty,
				e.ProbeSideTupleFetcher.needScanRowTableAfterProbeDone,
				e.ProbeSideTupleFetcher.shouldLimitProbeFetchSize(),
				&e.ProbeSideTupleFetcher.hashJoinCtxBase)
		}
		e.workerWg.RunWithRecover(fetchProbeSideChunksFunc, e.ProbeSideTupleFetcher.handleProbeSideFetcherPanic)
	}
}

func (e *HashJoinV2Exec) startProbeJoinWorkers(ctx context.Context) {
	var start time.Time
	if e.HashJoinCtxV2.stats != nil {
		start = time.Now()
	}

	if e.inRestore {
		// Wait for the restore build
		err := <-e.buildFinished
		if err != nil {
			return
		}
	}

	for i := uint(0); i < e.Concurrency; i++ {
		workerID := i
		e.workerWg.RunWithRecover(func() {
			defer trace.StartRegion(ctx, "HashJoinWorker").End()
			if e.inRestore {
				e.ProbeWorkers[workerID].restoreAndProbe(e.restoredProbeInDisk[workerID], start)
			} else {
				e.ProbeWorkers[workerID].runJoinWorker(start)
			}
		}, e.ProbeWorkers[workerID].handleProbeWorkerPanic)
	}
}

func (e *HashJoinV2Exec) fetchAndProbeHashTable(ctx context.Context) {
	start := time.Now()
	e.startProbeFetcher(ctx)

	// Join workers directly read data from disk when we are in restore status
	// and read data from fetcher otherwise.
	e.startProbeJoinWorkers(ctx)

	e.waiterWg.RunWithRecover(
		func() {
			e.waitJoinWorkers(start)
		}, nil)
}

func (w *ProbeWorkerV2) handleProbeWorkerPanic(r any) {
	if r != nil {
		w.HashJoinCtx.joinResultCh <- &hashjoinWorkerResult{err: util.GetRecoverError(r)}
	}
}

func (e *HashJoinV2Exec) handleJoinWorkerPanic(r any) {
	if r != nil {
		e.joinResultCh <- &hashjoinWorkerResult{err: util.GetRecoverError(r)}
	}
}

func (e *HashJoinV2Exec) waitJoinWorkers(start time.Time) {
	e.workerWg.Wait()
	if e.stats != nil {
		e.HashJoinCtxV2.stats.fetchAndProbe += int64(time.Since(start))
		for _, prober := range e.ProbeWorkers {
			e.stats.probeCollision += int64(prober.JoinProbe.GetProbeCollision())
		}
	}

	if !e.ProbeSideTupleFetcher.canSkipScanRowTable {
		if e.ProbeWorkers[0] != nil && e.ProbeWorkers[0].JoinProbe.NeedScanRowTable() {
			for i := uint(0); i < e.Concurrency; i++ {
				var workerID = i
				e.workerWg.RunWithRecover(func() {
					e.ProbeWorkers[workerID].scanRowTableAfterProbeDone()
				}, e.handleJoinWorkerPanic)
			}
			e.workerWg.Wait()
		}
	}
}

func (w *ProbeWorkerV2) scanRowTableAfterProbeDone() {
	w.JoinProbe.InitForScanRowTable()
	ok, joinResult := w.getNewJoinResult()
	if !ok {
		return
	}
	for !w.JoinProbe.IsScanRowTableDone() {
		joinResult = w.JoinProbe.ScanRowTable(joinResult, &w.HashJoinCtx.SessCtx.GetSessionVars().SQLKiller)
		if joinResult.err != nil {
			w.HashJoinCtx.joinResultCh <- joinResult
			return
		}

		err := triggerIntest(4)
		if err != nil {
			w.HashJoinCtx.joinResultCh <- &hashjoinWorkerResult{err: err}
			return
		}

		if joinResult.chk.IsFull() {
			w.HashJoinCtx.joinResultCh <- joinResult
			ok, joinResult = w.getNewJoinResult()
			if !ok {
				return
			}
		}
	}

	if joinResult.err != nil || (joinResult.chk != nil && joinResult.chk.NumRows() > 0) {
		w.HashJoinCtx.joinResultCh <- joinResult
	} else if joinResult.chk != nil && joinResult.chk.NumRows() == 0 {
		w.joinChkResourceCh <- joinResult.chk
	}
}

func (w *ProbeWorkerV2) processOneRestoredProbeChunk(joinResult *hashjoinWorkerResult) (ok bool, waitTime int64, _ *hashjoinWorkerResult) {
	joinResult.err = w.JoinProbe.SetRestoredChunkForProbe(w.restoredChkBuf)
	if joinResult.err != nil {
		return false, 0, joinResult
	}
	return w.probeAndSendResult(joinResult)
}

func (w *ProbeWorkerV2) processOneProbeChunk(probeChunk *chunk.Chunk, joinResult *hashjoinWorkerResult) (ok bool, waitTime int64, _ *hashjoinWorkerResult) {
	joinResult.err = w.JoinProbe.SetChunkForProbe(probeChunk)
	if joinResult.err != nil {
		return false, 0, joinResult
	}
	return w.probeAndSendResult(joinResult)
}

func (w *ProbeWorkerV2) probeAndSendResult(joinResult *hashjoinWorkerResult) (bool, int64, *hashjoinWorkerResult) {
	if w.HashJoinCtx.spillHelper.areAllPartitionsSpilled() {
		if intest.InTest && w.HashJoinCtx.spillHelper.hashJoinExec.inRestore {
			w.HashJoinCtx.spillHelper.skipProbeInRestoreForTest.Store(true)
		}
		return true, 0, joinResult
	}

	var ok bool
	waitTime := int64(0)
	for !w.JoinProbe.IsCurrentChunkProbeDone() {
		ok, joinResult = w.JoinProbe.Probe(joinResult, &w.HashJoinCtx.SessCtx.GetSessionVars().SQLKiller)
		if !ok || joinResult.err != nil {
			return ok, waitTime, joinResult
		}

		failpoint.Inject("processOneProbeChunkPanic", nil)
		if joinResult.chk.IsFull() {
			waitStart := time.Now()
			w.HashJoinCtx.joinResultCh <- joinResult
			ok, joinResult = w.getNewJoinResult()
			waitTime += int64(time.Since(waitStart))
			if !ok {
				return false, waitTime, joinResult
			}
		}
	}
	return true, waitTime, joinResult
}

func (w *ProbeWorkerV2) runJoinWorker(start time.Time) {
	probeTime := int64(0)
	if w.HashJoinCtx.stats != nil {
		defer func() {
			w.updateProbeStatistic(start, probeTime)
		}()
	}

	var (
		probeSideResult *chunk.Chunk
	)
	ok, joinResult := w.getNewJoinResult()
	if !ok {
		return
	}

	// Read and filter probeSideResult, and join the probeSideResult with the build side rows.
	emptyProbeSideResult := &probeChkResource{
		dest: w.probeResultCh,
	}
	for ok := true; ok; {
		select {
		case <-w.HashJoinCtx.closeCh:
			return
		case probeSideResult, ok = <-w.probeResultCh:
		}
		failpoint.Inject("ConsumeRandomPanic", nil)
		if !ok {
			break
		}

		err := triggerIntest(2)
		if err != nil {
			joinResult.err = err
			break
		}

		start := time.Now()
		waitTime := int64(0)
		ok, waitTime, joinResult = w.processOneProbeChunk(probeSideResult, joinResult)
		probeTime += int64(time.Since(start)) - waitTime
		if !ok {
			break
		}
		probeSideResult.Reset()
		emptyProbeSideResult.chk = probeSideResult

		// Give back to probe fetcher
		w.probeChkResourceCh <- emptyProbeSideResult
	}

	err := w.JoinProbe.SpillRemainingProbeChunks()
	if err != nil {
		joinResult.err = err
	}

	if joinResult.err != nil || (joinResult.chk != nil && joinResult.chk.NumRows() > 0) {
		w.HashJoinCtx.joinResultCh <- joinResult
	} else if joinResult.chk != nil && joinResult.chk.NumRows() == 0 {
		w.joinChkResourceCh <- joinResult.chk
	}
}

func (w *ProbeWorkerV2) getNewJoinResult() (bool, *hashjoinWorkerResult) {
	joinResult := &hashjoinWorkerResult{
		src: w.joinChkResourceCh,
	}
	ok := true
	select {
	case <-w.HashJoinCtx.closeCh:
		ok = false
	case joinResult.chk, ok = <-w.joinChkResourceCh:
	}
	return ok, joinResult
}

func (e *HashJoinV2Exec) reset() {
	e.resetProbeStatus()
	e.releaseDisk()
	e.resetHashTableContextForRestore()
	e.spillHelper.setCanSpillFlag(true)
	if e.HashJoinCtxV2.stats != nil {
		e.HashJoinCtxV2.stats.resetCurrentRound()
	}
}

func (e *HashJoinV2Exec) collectSpillStats() {
	if e.stats == nil || !e.spillHelper.isSpillTriggered() {
		return
	}

	round := e.spillHelper.round
	if len(e.stats.spill.totalSpillBytesPerRound) < round+1 {
		e.stats.spill.totalSpillBytesPerRound = append(e.stats.spill.totalSpillBytesPerRound, 0)
		e.stats.spill.spillBuildRowTableBytesPerRound = append(e.stats.spill.spillBuildRowTableBytesPerRound, 0)
		e.stats.spill.spillBuildHashTableBytesPerRound = append(e.stats.spill.spillBuildHashTableBytesPerRound, 0)
		e.stats.spill.spilledPartitionNumPerRound = append(e.stats.spill.spilledPartitionNumPerRound, 0)
	}

	buildRowTableSpillBytes := e.spillHelper.getBuildSpillBytes()
	buildHashTableSpillBytes := getHashTableMemoryUsage(getHashTableLengthByRowLen(e.spillHelper.spilledValidRowNum.Load()))
	probeSpillBytes := e.spillHelper.getProbeSpillBytes()
	spilledPartitionNum := e.spillHelper.getSpilledPartitionsNum()

	e.stats.spill.spillBuildRowTableBytesPerRound[round] += buildRowTableSpillBytes
	e.stats.spill.spillBuildHashTableBytesPerRound[round] += buildHashTableSpillBytes
	e.stats.spill.totalSpillBytesPerRound[round] += buildRowTableSpillBytes + probeSpillBytes
	e.stats.spill.spilledPartitionNumPerRound[round] += spilledPartitionNum
}

func (e *HashJoinV2Exec) startBuildAndProbe(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			e.joinResultCh <- &hashjoinWorkerResult{err: util.GetRecoverError(r)}
		}
		close(e.joinResultCh)
	}()

	lastRound := 0
	for {
		if e.finished.Load() {
			return
		}

		e.buildFinished = make(chan error, 1)

		e.fetchAndBuildHashTable(ctx)
		e.fetchAndProbeHashTable(ctx)

		e.waiterWg.Wait()
		e.collectSpillStats()
		e.reset()

		e.spillHelper.spillRoundForTest = max(e.spillHelper.spillRoundForTest, lastRound)
		err := e.spillHelper.prepareForRestoring(lastRound)
		if err != nil {
			e.joinResultCh <- &hashjoinWorkerResult{err: err}
			return
		}

		restoredPartition := e.spillHelper.stack.pop()
		if restoredPartition == nil {
			// No more data to restore
			return
		}
		e.spillHelper.round = restoredPartition.round

		if e.memTracker.BytesConsumed() != 0 {
			e.isMemoryClearedForTest = false
		}

		lastRound = restoredPartition.round
		e.restoredBuildInDisk = restoredPartition.buildSideChunks
		e.restoredProbeInDisk = restoredPartition.probeSideChunks

		if e.stats != nil && e.stats.spill.round < lastRound {
			e.stats.spill.round = lastRound
		}

		e.inRestore = true
	}
}

func (e *HashJoinV2Exec) resetProbeStatus() {
	for _, probe := range e.ProbeWorkers {
		probe.JoinProbe.ResetProbe()
	}
}

func (e *HashJoinV2Exec) releaseDisk() {
	if e.restoredBuildInDisk != nil {
		for _, inDisk := range e.restoredBuildInDisk {
			inDisk.Close()
		}
		e.restoredBuildInDisk = nil
	}

	if e.restoredProbeInDisk != nil {
		for _, inDisk := range e.restoredProbeInDisk {
			inDisk.Close()
		}
		e.restoredProbeInDisk = nil
	}
}

// Next implements the Executor Next interface.
// hash join constructs the result following these steps:
// step 1. fetch data from build side child and build a hash table;
// step 2. fetch data from probe child in a background goroutine and probe the hash table in multiple join workers.
func (e *HashJoinV2Exec) Next(ctx context.Context, req *chunk.Chunk) (err error) {
	if !e.prepared {
		e.initHashTableContext()
		e.initializeForProbe()
		e.spillHelper.setCanSpillFlag(true)
		e.buildFinished = make(chan error, 1)
		e.hashTableContext.memoryTracker.AttachTo(e.memTracker)
		go e.startBuildAndProbe(ctx)
		e.prepared = true
	}
	if e.ProbeSideTupleFetcher.shouldLimitProbeFetchSize() {
		atomic.StoreInt64(&e.ProbeSideTupleFetcher.requiredRows, int64(req.RequiredRows()))
	}
	req.Reset()

	result, ok := <-e.joinResultCh
	if !ok {
		return nil
	}
	if result.err != nil {
		e.finished.Store(true)
		return result.err
	}
	req.SwapColumns(result.chk)
	result.src <- result.chk
	return nil
}

func (e *HashJoinV2Exec) handleFetchAndBuildHashTablePanic(r any) {
	if r != nil {
		e.buildFinished <- util.GetRecoverError(r)
	}
	close(e.buildFinished)
}

// checkBalance checks whether the segment count of each partition is balanced.
func (e *HashJoinV2Exec) checkBalance(totalSegmentCnt int) bool {
	isBalanced := e.Concurrency == e.partitionNumber
	if !isBalanced {
		return false
	}
	avgSegCnt := totalSegmentCnt / int(e.partitionNumber)
	balanceThreshold := int(float64(avgSegCnt) * 0.8)
	subTables := e.HashJoinCtxV2.hashTableContext.hashTable.tables

	for _, subTable := range subTables {
		if math.Abs(float64(len(subTable.rowData.segments)-avgSegCnt)) > float64(balanceThreshold) {
			isBalanced = false
			break
		}
	}
	return isBalanced
}

func (e *HashJoinV2Exec) createTasks(buildTaskCh chan<- *buildTask, totalSegmentCnt int, doneCh chan struct{}) {
	isBalanced := e.checkBalance(totalSegmentCnt)
	segStep := max(1, totalSegmentCnt/int(e.Concurrency))
	subTables := e.HashJoinCtxV2.hashTableContext.hashTable.tables
	createBuildTask := func(partIdx int, segStartIdx int, segEndIdx int) *buildTask {
		return &buildTask{partitionIdx: partIdx, segStartIdx: segStartIdx, segEndIdx: segEndIdx}
	}
	failpoint.Inject("createTasksPanic", nil)

	if isBalanced {
		for partIdx, subTable := range subTables {
			_ = triggerIntest(5)
			segmentsLen := len(subTable.rowData.segments)
			select {
			case <-doneCh:
				return
			case buildTaskCh <- createBuildTask(partIdx, 0, segmentsLen):
			}
		}
		return
	}

	partitionStartIndex := make([]int, len(subTables))
	partitionSegmentLength := make([]int, len(subTables))
	for i := 0; i < len(subTables); i++ {
		partitionStartIndex[i] = 0
		partitionSegmentLength[i] = len(subTables[i].rowData.segments)
	}

	for {
		hasNewTask := false
		for partIdx := range subTables {
			// create table by round-robin all the partitions so the build thread is likely to build different partition at the same time
			if partitionStartIndex[partIdx] < partitionSegmentLength[partIdx] {
				startIndex := partitionStartIndex[partIdx]
				endIndex := min(startIndex+segStep, partitionSegmentLength[partIdx])
				select {
				case <-doneCh:
					return
				case buildTaskCh <- createBuildTask(partIdx, startIndex, endIndex):
				}
				partitionStartIndex[partIdx] = endIndex
				hasNewTask = true
			}
		}
		if !hasNewTask {
			break
		}
	}
}

func (e *HashJoinV2Exec) fetchAndBuildHashTable(ctx context.Context) {
	e.workerWg.RunWithRecover(func() {
		defer trace.StartRegion(ctx, "HashJoinHashTableBuilder").End()
		e.fetchAndBuildHashTableImpl(ctx)
	}, e.handleFetchAndBuildHashTablePanic)
}

func (e *HashJoinV2Exec) fetchAndBuildHashTableImpl(ctx context.Context) {
	if e.stats != nil {
		start := time.Now()
		defer func() {
			e.stats.fetchAndBuildHashTable += int64(time.Since(start))
		}()
	}

	waitJobDone := func(wg *sync.WaitGroup, errCh chan error) bool {
		wg.Wait()
		close(errCh)
		if err := <-errCh; err != nil {
			e.buildFinished <- err
			return false
		}
		return true
	}

	// It's useful when spill is triggered and the fetcher could know when workers finish their works.
	fetcherAndWorkerSyncer := &sync.WaitGroup{}
	wg := new(sync.WaitGroup)
	errCh := make(chan error, 1+e.Concurrency)

	// doneCh is used by the consumer(splitAndAppendToRowTable) to info the producer(fetchBuildSideRows) that the consumer meet error and stop consume data
	doneCh := make(chan struct{}, e.Concurrency)
	// init builder, todo maybe the builder can be reused during the whole life cycle of the executor
	hashJoinCtx := e.HashJoinCtxV2
	for _, worker := range e.BuildWorkers {
		worker.builder = createRowTableBuilder(worker.BuildKeyColIdx, hashJoinCtx.BuildKeyTypes, hashJoinCtx.partitionNumber, worker.HasNullableKey, hashJoinCtx.BuildFilter != nil, hashJoinCtx.needScanRowTableAfterProbeDone)
	}
	srcChkCh, waitForController := e.fetchBuildSideRows(ctx, fetcherAndWorkerSyncer, wg, errCh, doneCh)
	e.splitAndAppendToRowTable(srcChkCh, waitForController, fetcherAndWorkerSyncer, wg, errCh, doneCh)
	success := waitJobDone(wg, errCh)
	if !success {
		return
	}

	if e.spillHelper.spillTriggered {
		e.spillHelper.spillTriggedInBuildingStageForTest = true
	}

	totalSegmentCnt, err := e.hashTableContext.mergeRowTablesToHashTable(e.partitionNumber, e.spillHelper)
	if err != nil {
		e.buildFinished <- err
		return
	}

	wg = new(sync.WaitGroup)
	errCh = make(chan error, 1+e.Concurrency)
	// doneCh is used by the consumer(buildHashTable) to info the producer(createBuildTasks) that the consumer meet error and stop consume data
	doneCh = make(chan struct{}, e.Concurrency)

	buildTaskCh := e.createBuildTasks(totalSegmentCnt, wg, errCh, doneCh)
	e.buildHashTable(buildTaskCh, wg, errCh, doneCh)
	waitJobDone(wg, errCh)
}

func (e *HashJoinV2Exec) fetchBuildSideRows(ctx context.Context, fetcherAndWorkerSyncer *sync.WaitGroup, wg *sync.WaitGroup, errCh chan error, doneCh chan struct{}) (chan *chunk.Chunk, chan struct{}) {
	srcChkCh := make(chan *chunk.Chunk, 1)
	var waitForController chan struct{}
	if e.inRestore {
		waitForController = make(chan struct{})
	}

	wg.Add(1)
	e.workerWg.RunWithRecover(
		func() {
			defer trace.StartRegion(ctx, "HashJoinBuildSideFetcher").End()
			if e.inRestore {
				chunkNum := e.getRestoredBuildChunkNum()
				e.controlWorkersForRestore(chunkNum, srcChkCh, waitForController, fetcherAndWorkerSyncer, errCh, doneCh)
			} else {
				fetcher := e.BuildWorkers[0]
				fetcher.fetchBuildSideRows(ctx, &fetcher.HashJoinCtx.hashJoinCtxBase, fetcherAndWorkerSyncer, e.spillHelper, srcChkCh, errCh, doneCh)
			}
		},
		func(r any) {
			if r != nil {
				errCh <- util.GetRecoverError(r)
			}
			wg.Done()
		},
	)
	return srcChkCh, waitForController
}

func (e *HashJoinV2Exec) getRestoredBuildChunkNum() int {
	chunkNum := 0
	for _, inDisk := range e.restoredBuildInDisk {
		chunkNum += inDisk.NumChunks()
	}
	return chunkNum
}

func (e *HashJoinV2Exec) controlWorkersForRestore(chunkNum int, syncCh chan *chunk.Chunk, waitForController chan struct{}, fetcherAndWorkerSyncer *sync.WaitGroup, errCh chan<- error, doneCh <-chan struct{}) {
	defer func() {
		close(syncCh)

		hasError := false
		if r := recover(); r != nil {
			errCh <- util.GetRecoverError(r)
			hasError = true
		}

		fetcherAndWorkerSyncer.Wait()

		// Spill remaining rows
		if !hasError && e.spillHelper.isSpillTriggered() {
			err := e.spillHelper.spillRemainingRows()
			if err != nil {
				errCh <- err
			}
		}

		// Tell workers that they can execute `appendRemainingRowLocations` function
		close(waitForController)
	}()

	for i := 0; i < chunkNum; i++ {
		if e.finished.Load() {
			return
		}

		err := checkAndSpillRowTableIfNeeded(fetcherAndWorkerSyncer, e.spillHelper)
		if err != nil {
			errCh <- err
			return
		}

		err = triggerIntest(2)
		if err != nil {
			errCh <- err
			return
		}

		fetcherAndWorkerSyncer.Add(1)
		select {
		case <-doneCh:
			fetcherAndWorkerSyncer.Done()
			return
		case <-e.hashJoinCtxBase.closeCh:
			fetcherAndWorkerSyncer.Done()
			return
		case syncCh <- nil:
		}
	}
}

func handleErr(err error, errCh chan error, doneCh chan struct{}) {
	errCh <- err
	doneCh <- struct{}{}
}

func (e *HashJoinV2Exec) splitAndAppendToRowTable(srcChkCh chan *chunk.Chunk, waitForController chan struct{}, fetcherAndWorkerSyncer *sync.WaitGroup, wg *sync.WaitGroup, errCh chan error, doneCh chan struct{}) {
	wg.Add(int(e.Concurrency))
	for i := uint(0); i < e.Concurrency; i++ {
		workIndex := i
		e.workerWg.RunWithRecover(
			func() {
				if e.inRestore {
					e.BuildWorkers[workIndex].splitPartitionAndAppendToRowTableForRestore(e.restoredBuildInDisk[workIndex], srcChkCh, waitForController, fetcherAndWorkerSyncer, errCh, doneCh)
				} else {
					e.BuildWorkers[workIndex].splitPartitionAndAppendToRowTable(e.SessCtx.GetSessionVars().StmtCtx.TypeCtx(), fetcherAndWorkerSyncer, srcChkCh, errCh, doneCh)
				}
			},
			func(r any) {
				if r != nil {
					errCh <- util.GetRecoverError(r)
					doneCh <- struct{}{}
				}
				wg.Done()
			},
		)
	}
}

func (e *HashJoinV2Exec) createBuildTasks(totalSegmentCnt int, wg *sync.WaitGroup, errCh chan error, doneCh chan struct{}) chan *buildTask {
	buildTaskCh := make(chan *buildTask, e.Concurrency)
	wg.Add(1)
	e.workerWg.RunWithRecover(
		func() { e.createTasks(buildTaskCh, totalSegmentCnt, doneCh) },
		func(r any) {
			if r != nil {
				errCh <- util.GetRecoverError(r)
			}
			close(buildTaskCh)
			wg.Done()
		},
	)
	return buildTaskCh
}

func (e *HashJoinV2Exec) buildHashTable(buildTaskCh chan *buildTask, wg *sync.WaitGroup, errCh chan error, doneCh chan struct{}) {
	for i := uint(0); i < e.Concurrency; i++ {
		wg.Add(1)
		workID := i
		e.workerWg.RunWithRecover(
			func() {
				err := e.BuildWorkers[workID].buildHashTable(buildTaskCh)
				if err != nil {
					errCh <- err
					doneCh <- struct{}{}
				}
			},
			func(r any) {
				if r != nil {
					errCh <- util.GetRecoverError(r)
					doneCh <- struct{}{}
				}
				wg.Done()
			},
		)
	}
}

type buildTask struct {
	partitionIdx int
	segStartIdx  int
	segEndIdx    int
}

func generatePartitionIndex(hashValue uint64, partitionMaskOffset int) uint64 {
	return hashValue >> uint64(partitionMaskOffset)
}

func getProbeSpillChunkFieldTypes(probeFieldTypes []*types.FieldType) []*types.FieldType {
	ret := make([]*types.FieldType, 0, len(probeFieldTypes)+2)
	hashValueField := types.NewFieldType(mysql.TypeLonglong)
	hashValueField.AddFlag(mysql.UnsignedFlag)
	ret = append(ret, hashValueField)                    // hash value
	ret = append(ret, types.NewFieldType(mysql.TypeBit)) // serialized key
	ret = append(ret, probeFieldTypes...)                // row data
	return ret
}

func rehash(oldHashValue uint64, rehashBuf []byte, hash hash.Hash64) uint64 {
	*(*uint64)(unsafe.Pointer(&rehashBuf[0])) = oldHashValue

	hash.Reset()
	hash.Write(rehashBuf)
	return hash.Sum64()
}

func issue59377Intest(err *error) {
	failpoint.Inject("Issue59377", func() {
		*err = errors.New("Random failpoint error is triggered")
	})
}

func triggerIntest(errProbability int) error {
	failpoint.Inject("slowWorkers", func(val failpoint.Value) {
		if val.(bool) {
			num := rand.Intn(100000)
			if num < 2 {
				time.Sleep(time.Duration(num) * time.Millisecond)
			}
		}
	})

	var err error
	failpoint.Inject("panicOrError", func(val failpoint.Value) {
		if val.(bool) {
			num := rand.Intn(100000)
			if num < errProbability/2 {
				panic("Random failpoint panic")
			} else if num < errProbability {
				err = errors.New("Random failpoint error is triggered")
			}
		}
	})

	return err
}
