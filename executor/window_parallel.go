// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package executor

import (
	"context"
	"sync"

	"github.com/cznic/mathutil"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/logutil"
	"go.uber.org/zap"
)

// Run WindowExec in a parallel manner.
//
//                            +-------------+
//                            | Main Thread |
//                            +------+------+
//                                   ^
//                                   |
//                                   +
//                                  +++
//                                  | |  groupingOutputCh (1 x Concurrency)
//                                  +++
//                                   ^
//                                   |
//                               +---+-----------+
//                               |               |
//                 +--------------+             +--------------+
//                 | group worker |     ......  | group worker |  group worker (x Concurrency): Consuming GroupRows
//                 +------------+-+             +-+------------+
//                              ^                 ^
//                              |                 |
//                             +-+  +-+  ......  +-+
//                             | |  | |          | |
//                             ...  ...          ...    groupingInputChs (Concurrency x 1)
//                             | |  | |          | |
//                             +++  +++          +++
//                              ^    ^            ^
//                              |    |            |
//                     +--------o----+            |
//                     |        +-----------------+---+
//                     |                              |
//                 +---+------------+------------+----+-----------+
//                 |          groupChecker.splitIntoGroups        |
//                 +--------------+-+------------+-+--------------+
//                                        ^
//                                        |
//                        +---------------v-----------------+
//           inputCh ---> |  data fetcher (of SORTED input) |
//     (1 x Concurrency)  +---------------------------------+
//
////////////////////////////////////////////////////////////////////////////////////////

type windowParallelExec struct {
	// isChildReturnEmpty indicates whether the child executor only returns an empty input.
	isChildReturnEmpty bool
	prepared           bool
	finishCh           chan struct{}

	inputCh          chan *windowParallelInput
	groupingInputChs []chan *windowGroupData
	groupingOutputCh chan *windowGroupingOutput

	groupingWorkers []windowGroupingWorker
}

type windowParallelInput struct {
	groupData  *windowGroupData
	giveBackCh chan<- *windowGroupData
}

type windowGroupData struct {
	groupRows []chunk.Row
}

type windowGroupingOutput struct {
	chk        *chunk.Chunk
	err        error
	giveBackCh chan *chunk.Chunk
}

type windowGroupingWorker struct {
	ctx       sessionctx.Context
	processor windowProcessor
	finishCh  <-chan struct{}

	inputCh              chan *windowGroupData
	giveBackCh           chan<- *windowParallelInput
	outputCh             chan *windowGroupingOutput
	outputResultHolderCh chan *chunk.Chunk

	groupRows             []chunk.Row
	result                *chunk.Chunk
	remainingRowsInResult int
}

func (e *WindowExec) isParallelExec() bool {
	return e.groupingConcurrency > 1
}

func (e *WindowExec) initForParallelExec() {
	e.prepared = false
	concurrency := e.groupingConcurrency

	e.isChildReturnEmpty = true
	e.finishCh = make(chan struct{}, 1)

	e.inputCh = make(chan *windowParallelInput, concurrency)
	e.groupingInputChs = make([]chan *windowGroupData, concurrency)
	for i := range e.groupingInputChs {
		e.groupingInputChs[i] = make(chan *windowGroupData, 1)
	}
	e.groupingOutputCh = make(chan *windowGroupingOutput, concurrency)
	e.groupingWorkers = make([]windowGroupingWorker, concurrency)

	for i := 0; i < concurrency; i++ {
		var processor windowProcessor
		if i == 0 {
			processor = e.processor
		} else {
			processor = e.processor.cloneWithNewPartialResult()
		}
		w := windowGroupingWorker{
			ctx:                  e.ctx,
			processor:            processor,
			finishCh:             e.finishCh,
			inputCh:              e.groupingInputChs[i],
			giveBackCh:           e.inputCh,
			outputCh:             e.groupingOutputCh,
			outputResultHolderCh: make(chan *chunk.Chunk, 1),
		}

		e.groupingWorkers[i] = w
		e.inputCh <- &windowParallelInput{
			groupData:  &windowGroupData{},
			giveBackCh: w.inputCh,
		}
		w.outputResultHolderCh <- newFirstChunk(e)
	}
}

func (e *WindowExec) deinitForParallelExec() {
	if !e.prepared {
		close(e.inputCh)                        // TODO: why ?
		for _, ch := range e.groupingInputChs { // DONE by execDataFetcher.defer if prepared
			close(ch)
		}
		close(e.groupingOutputCh) // DONE by waitWorkerAndCloseOutput if prepared
	}
	close(e.finishCh)
	for _, ch := range e.groupingInputChs {
		for range ch {
		}
	}
	for range e.groupingOutputCh {
	}
	e.executed = false

	if e.runtimeStats != nil { // TODO: why ?
		e.runtimeStats.SetConcurrencyInfo("WindowGroupingConcurrency", e.groupingConcurrency)
	}
}

func recoveryWindowParallelExec(output chan *windowGroupingOutput, r interface{}) {
	err := errors.Errorf("%v", r)
	output <- &windowGroupingOutput{err: errors.Errorf("%v", r)}
	logutil.BgLogger().Error("parallel window grouping panicked", zap.Error(err))
}

func (e *WindowExec) execDataFetcher(ctx context.Context) {
	var (
		input     *windowParallelInput
		groupData *windowGroupData
		ok        bool
		eof       bool
		err       error
	)
	defer func() {
		if r := recover(); r != nil {
			recoveryWindowParallelExec(e.groupingOutputCh, r)
		}
		for i := range e.groupingInputChs {
			close(e.groupingInputChs[i])
		}
	}()
	for {
		select {
		case <-e.finishCh:
			return
		case input, ok = <-e.inputCh:
			if !ok {
				return
			}
			groupData = input.groupData
		}
		groupData.groupRows, eof, err = e.fetchOneGroup(ctx)
		if err != nil {
			e.groupingOutputCh <- &windowGroupingOutput{err: err}
			return
		}
		if eof {
			return
		}
		input.giveBackCh <- groupData
	}
}

func (e *WindowExec) prepare4ParallelExec(ctx context.Context) {
	go e.execDataFetcher(ctx)

	waitGroup := &sync.WaitGroup{}
	waitGroup.Add(len(e.groupingWorkers))
	for i := range e.groupingWorkers {
		go e.groupingWorkers[i].run(e.ctx, waitGroup)
	}

	go e.waitWorkerAndCloseOutput(waitGroup)
}

func (e *WindowExec) waitWorkerAndCloseOutput(waitGroup *sync.WaitGroup) {
	waitGroup.Wait()
	close(e.groupingOutputCh)
}

func (e *WindowExec) parallelExec(ctx context.Context, chk *chunk.Chunk) error {
	if !e.prepared {
		e.prepare4ParallelExec(ctx)
		e.prepared = true
	}

	failpoint.Inject("parallelWindowError", func(val failpoint.Value) {
		if val.(bool) {
			failpoint.Return(errors.New("WindowExec.parallelExec error"))
		}
	})

	if e.executed {
		return nil
	}
	for {
		result, ok := <-e.groupingOutputCh
		if !ok {
			e.executed = true
			return nil
		}
		if result.err != nil {
			return result.err
		}
		chk.SwapColumns(result.chk)
		result.chk.Reset()
		result.giveBackCh <- result.chk
		if chk.NumRows() > 0 {
			return nil
		}
	}
}

func (w *windowGroupingWorker) run(ctx sessionctx.Context, waitGroup *sync.WaitGroup) {
	defer func() {
		if r := recover(); r != nil {
			recoveryWindowParallelExec(w.outputCh, r)
		}
		waitGroup.Done()
	}()

	var finished bool
	w.result, finished = w.receiveOutputResultHolder()
	if finished {
		return
	}
	w.remainingRowsInResult = w.result.RequiredRows()

	for {
		groupRows, ok := w.getInput()
		if !ok {
			if w.result.NumRows() > 0 {
				w.outputCh <- &windowGroupingOutput{chk: w.result, giveBackCh: w.outputResultHolderCh}
			}
			return
		}
		err := w.consumeGroupRows(groupRows)
		if err != nil {
			w.outputCh <- &windowGroupingOutput{err: err}
			return
		}
	}
}

func (w *windowGroupingWorker) consumeGroupRows(groupRows []chunk.Row) (err error) {
	remainingRowsInGroup := len(groupRows)
	if remainingRowsInGroup == 0 {
		return nil
	}
	for remainingRowsInGroup > 0 {
		remained := mathutil.Min(w.remainingRowsInResult, remainingRowsInGroup)
		w.remainingRowsInResult -= remained
		remainingRowsInGroup -= remained

		groupRows, err = w.processor.consumeGroupRows(w.ctx, groupRows)
		if err != nil {
			return errors.Trace(err)
		}
		_, err = w.processor.appendResult2Chunk(w.ctx, groupRows, w.result, remained)
		if err != nil {
			return errors.Trace(err)
		}
		if w.result.IsFull() {
			w.outputCh <- &windowGroupingOutput{chk: w.result, giveBackCh: w.outputResultHolderCh}
			var finished bool
			w.result, finished = w.receiveOutputResultHolder()
			if finished {
				return nil
			}
			w.remainingRowsInResult = w.result.RequiredRows()
		}
	}
	w.processor.resetPartialResult()
	return nil
}

func (w *windowGroupingWorker) receiveOutputResultHolder() (*chunk.Chunk, bool) {
	select {
	case <-w.finishCh:
		return nil, true
	case result, ok := <-w.outputResultHolderCh:
		return result, !ok
	}
}

func (w *windowGroupingWorker) getInput() ([]chunk.Row, bool) {
	select {
	case <-w.finishCh:
		return nil, false
	case groupData, ok := <-w.inputCh:
		if !ok {
			return nil, false
		}
		groupRows := groupData.groupRows
		groupData.groupRows = nil
		w.giveBackCh <- &windowParallelInput{
			groupData:  groupData,
			giveBackCh: w.inputCh,
		}
		return groupRows, true
	}
}
