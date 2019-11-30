// Copyright 2018 PingCAP, Inc.
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
	"encoding/hex"

	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/table/tables"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/logutil"
	"go.uber.org/zap"
)

type vecUpdateDupChunk struct {
	numColumns      int
	toBeCheckedRows []toBeCheckedRow
	handles         []int64
	input           *chunk.Chunk
	output          *chunk.Chunk
}

func (v *vecUpdateDupChunk) Reset() {
	v.toBeCheckedRows = v.toBeCheckedRows[:0]
	v.handles = v.handles[:0]
	v.input.Reset()
	v.output.Reset()
}

func (v *vecUpdateDupChunk) Append(handle int64, row chunk.Row) {
	v.handles = append(v.handles, handle)
	v.input.AppendRow(row)
}

func (v *vecUpdateDupChunk) IsFull() bool {
	return v.input.NumRows() >= v.input.Capacity()
}
func (v *vecUpdateDupChunk) IsEmpty() bool {
	return v.input.NumRows() == 0
}

func newVecUpdateDupChunk(numColumns int, ftypes []*types.FieldType, size int, maxChunkSize int) *vecUpdateDupChunk {
	return &vecUpdateDupChunk{
		numColumns:      numColumns,
		toBeCheckedRows: make([]toBeCheckedRow, 0, maxChunkSize),
		handles:         make([]int64, 0, maxChunkSize),
		input:           chunk.New(ftypes, size, maxChunkSize),
		output:          chunk.New(ftypes, size, maxChunkSize),
	}
}

// batchVecUpdateDupRows updates multi-rows in batch if they are duplicate with rows in table
// in a vectorized manner.
func (e *InsertExec) batchVecUpdateDupRows(ctx context.Context, txn kv.Transaction, newRows [][]types.Datum, toBeCheckedRows []toBeCheckedRow) error {
	// DEBUG //
	e.ctx.GetSessionVars().StmtCtx.DupKeyAsWarning = true

	chk := newVecUpdateDupChunk(len(e.Table.WritableCols()), e.evalBufferTypes, len(toBeCheckedRows), e.base().maxChunkSize)

	for i, r := range toBeCheckedRows {
		if r.handleKey != nil {
			handle, err := tablecodec.DecodeRowKey(r.handleKey.newKV.key)
			if err != nil {
				return err
			}

			err = e.vecUpdateDupRow(ctx, txn, r, handle, e.OnDuplicate, chk)
			if err == nil {
				continue
			}
			if !kv.IsErrNotFound(err) {
				return err
			}
		}

		for _, uk := range r.uniqueKeys {
			val, err := txn.Get(ctx, uk.newKV.key)
			if err != nil {
				if kv.IsErrNotFound(err) {
					continue
				}
				return err
			}
			handle, err := tables.DecodeHandle(val)
			if err != nil {
				return err
			}

			err = e.vecUpdateDupRow(ctx, txn, r, handle, e.OnDuplicate, chk)
			if err != nil {
				if kv.IsErrNotFound(err) {
					// Data index inconsistent? A unique key provide the handle information, but the
					// handle points to nothing.
					logutil.BgLogger().Error("get old row failed when insert on dup",
						zap.String("uniqueKey", hex.EncodeToString(uk.newKV.key)),
						zap.Int64("handle", handle),
						zap.String("toBeInsertedRow", types.DatumsToStrNoErr(r.row)))
				}
				return err
			}

			newRows[i] = nil
			break
		}

		// If row was checked with no duplicate keys,
		// we should do insert the row,
		// and key-values should be filled back to dupOldRowValues for the further row check,
		// due to there may be duplicate keys inside the insert statement.
		if newRows[i] != nil {
			if !chk.IsEmpty() {
				if err := e.doVecDupRowUpdateOneChunk(ctx, e.OnDuplicate, chk); err != nil {
					return err
				}
				chk.Reset()
			}

			_, err := e.addRecord(ctx, newRows[i])
			if err != nil {
				if e.ctx.GetSessionVars().StmtCtx.DupKeyAsWarning && kv.ErrKeyExists.Equal(err) {
					e.ctx.GetSessionVars().StmtCtx.AppendWarning(err)
				} else {
					return err
				}
			}
		}
	}

	if !chk.IsEmpty() {
		defer chk.Reset()
		if err := e.doVecDupRowUpdateOneChunk(ctx, e.OnDuplicate, chk); err != nil {
			return err
		}
	}

	return nil
}

func (e *InsertExec) vecUpdateDupRow(ctx context.Context, txn kv.Transaction, row toBeCheckedRow, handle int64, onDuplicate []*expression.Assignment, chk *vecUpdateDupChunk) error {
	oldRow, err := getOldRow(ctx, e.ctx, txn, row.t, handle, e.GenExprs)
	if err != nil {
		return err
	}

	return e.doVecDupRowUpdate(ctx, handle, oldRow, row.row, e.OnDuplicate, chk)
}

// doVecDupRowUpdate updates the duplicate row in a vectorized manner
func (e *InsertExec) doVecDupRowUpdate(ctx context.Context, handle int64, oldRow []types.Datum, newRow []types.Datum,
	cols []*expression.Assignment, chk *vecUpdateDupChunk) error {
	// NOTE: In order to execute the expression inside the column assignment,
	// we have to put the value of "oldRow" before "newRow" in "row4Update" to
	// be consistent with "Schema4OnDuplicate" in the "Insert" PhysicalPlan.
	e.row4Update = e.row4Update[:0]
	e.row4Update = append(e.row4Update, oldRow...)
	e.row4Update = append(e.row4Update, newRow...)

	// Update old row when the key is duplicated.
	e.evalBuffer4Dup.SetDatums(e.row4Update...)
	chk.Append(handle, e.evalBuffer4Dup.ToRow())
	if chk.IsFull() {
		defer chk.Reset()
		if err := e.doVecDupRowUpdateOneChunk(ctx, cols, chk); err != nil {
			return err
		}
	}
	return nil
}

// doVecDupRowUpdateOneChunk updates the duplicate row.
func (e *InsertExec) doVecDupRowUpdateOneChunk(ctx context.Context, cols []*expression.Assignment, chk *vecUpdateDupChunk) error {
	assignFlag := make([]bool, len(e.Table.WritableCols()))

	iter := chunk.NewIterator4Chunk(chk.input)
	for row := iter.Begin(); row != iter.End(); row = iter.Next() {
		datums := row.GetDatumRow(e.evalBufferTypes)
		oldRow := make([]types.Datum, chk.numColumns)
		copy(oldRow, datums[:chk.numColumns])
		newRow := datums[chk.numColumns:]
		handle := chk.handles[iter.Cursor()]

		// See http://dev.mysql.com/doc/refman/5.7/en/miscellaneous-functions.html#function_values
		e.curInsertVals.SetDatums(newRow...)
		e.ctx.GetSessionVars().CurrInsertValues = e.curInsertVals.ToRow()

		// NOTE: In order to execute the expression inside the column assignment,
		// we have to put the value of "oldRow" before "newRow" in "row4Update" to
		// be consistent with "Schema4OnDuplicate" in the "Insert" PhysicalPlan.
		e.row4Update = datums

		// Update old row when the key is duplicated.
		e.evalBuffer4Dup.SetDatums(e.row4Update...)
		for _, col := range cols {
			val, err1 := col.Expr.Eval(e.evalBuffer4Dup.ToRow())
			if err1 != nil {
				return err1
			}
			e.row4Update[col.Col.Index], err1 = table.CastValue(e.ctx, val, col.Col.ToInfo())
			if err1 != nil {
				return err1
			}
			e.evalBuffer4Dup.SetDatum(col.Col.Index, e.row4Update[col.Col.Index])
			assignFlag[col.Col.Index] = true
		}

		newData := e.row4Update[:len(oldRow)]
		_, _, _, err := updateRecord(ctx, e.ctx, handle, oldRow, newData, assignFlag, e.Table, true)
		if err != nil {
			if e.ctx.GetSessionVars().StmtCtx.DupKeyAsWarning && kv.ErrKeyExists.Equal(err) {
				e.ctx.GetSessionVars().StmtCtx.AppendWarning(err)
			} else {
				return err
			}
		}
	}
	return nil
}
