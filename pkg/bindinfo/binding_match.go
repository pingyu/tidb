// Copyright 2023 PingCAP, Inc.
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

package bindinfo

import (
	"strings"
	"sync"

	"github.com/pingcap/tidb/pkg/bindinfo/norm"
	"github.com/pingcap/tidb/pkg/metrics"
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/util/hint"
)

var (
	// GetGlobalBindingHandle is a function to get the global binding handle.
	// It is mainly used to resolve cycle import issue.
	GetGlobalBindingHandle func(sctx sessionctx.Context) GlobalBindingHandle
)

// BindingMatchInfo records necessary information for cross-db binding matching.
// This is mainly for plan cache to avoid normalizing the same statement repeatedly.
type BindingMatchInfo struct {
	NoDBDigest string
	TableNames []*ast.TableName
}

// MatchSQLBindingForPlanCache matches binding for plan cache.
func MatchSQLBindingForPlanCache(sctx sessionctx.Context, stmtNode ast.StmtNode, info *BindingMatchInfo) (bindingSQL string, ignoreBinding bool) {
	binding, matched, _ := matchSQLBinding(sctx, stmtNode, info)
	if matched {
		bindingSQL = binding.BindSQL
		ignoreBinding = binding.Hint.ContainTableHint(hint.HintIgnorePlanCache)
	}
	return
}

// MatchSQLBinding returns the matched binding for this statement.
func MatchSQLBinding(sctx sessionctx.Context, stmtNode ast.StmtNode) (binding *Binding, matched bool, scope string) {
	return matchSQLBinding(sctx, stmtNode, nil)
}

func matchSQLBinding(sctx sessionctx.Context, stmtNode ast.StmtNode, info *BindingMatchInfo) (binding *Binding, matched bool, scope string) {
	useBinding := sctx.GetSessionVars().UsePlanBaselines
	if !useBinding || stmtNode == nil {
		return
	}
	// When the domain is initializing, the bind will be nil.
	if sctx.Value(SessionBindInfoKeyType) == nil {
		return
	}

	// record the normalization result into info to avoid repeat normalization next time.
	var noDBDigest string
	var tableNames []*ast.TableName
	if info == nil || info.TableNames == nil || info.NoDBDigest == "" {
		_, noDBDigest = norm.NormalizeStmtForBinding(stmtNode, norm.WithoutDB(true))
		tableNames = CollectTableNames(stmtNode)
		if info != nil {
			info.NoDBDigest = noDBDigest
			info.TableNames = tableNames
		}
	} else {
		noDBDigest = info.NoDBDigest
		tableNames = info.TableNames
	}

	sessionHandle := sctx.Value(SessionBindInfoKeyType).(SessionBindingHandle)
	if binding, matched := sessionHandle.MatchSessionBinding(sctx, noDBDigest, tableNames); matched {
		return binding, matched, metrics.ScopeSession
	}
	globalHandle := GetGlobalBindingHandle(sctx)
	if globalHandle == nil {
		return
	}
	binding, matched = globalHandle.MatchGlobalBinding(sctx, noDBDigest, tableNames)
	if matched {
		return binding, matched, metrics.ScopeGlobal
	}

	return
}

func noDBDigestFromBinding(binding *Binding) (string, error) {
	p := parser.New()
	stmt, err := p.ParseOneStmt(binding.BindSQL, binding.Charset, binding.Collation)
	if err != nil {
		return "", err
	}
	_, bindingNoDBDigest := norm.NormalizeStmtForBinding(stmt, norm.WithoutDB(true))
	return bindingNoDBDigest, nil
}

func crossDBMatchBindings(sctx sessionctx.Context, tableNames []*ast.TableName, bindings []*Binding) (matchedBinding *Binding, isMatched bool) {
	leastWildcards := len(tableNames) + 1
	enableCrossDBBinding := sctx.GetSessionVars().EnableFuzzyBinding
	for _, binding := range bindings {
		numWildcards, matched := crossDBMatchBindingTableName(sctx.GetSessionVars().CurrentDB, tableNames, binding.TableNames)
		if matched && numWildcards > 0 && sctx != nil && !enableCrossDBBinding {
			continue // cross-db binding is disabled, skip this binding
		}
		if matched && numWildcards < leastWildcards {
			matchedBinding = binding
			isMatched = true
			leastWildcards = numWildcards
		}
	}
	return
}

func crossDBMatchBindingTableName(currentDB string, stmtTableNames, bindingTableNames []*ast.TableName) (numWildcards int, matched bool) {
	if len(stmtTableNames) != len(bindingTableNames) {
		return 0, false
	}
	for i := range stmtTableNames {
		if stmtTableNames[i].Name.L != bindingTableNames[i].Name.L {
			return 0, false
		}
		if bindingTableNames[i].Schema.L == "*" {
			numWildcards++
		}
		if bindingTableNames[i].Schema.L == stmtTableNames[i].Schema.L || // exactly same, or
			(stmtTableNames[i].Schema.L == "" && bindingTableNames[i].Schema.L == strings.ToLower(currentDB)) || // equal to the current DB, or
			bindingTableNames[i].Schema.L == "*" { // cross-db match successfully
			continue
		}
		return 0, false
	}
	return numWildcards, true
}

// isCrossDBBinding checks whether the stmtNode is a cross-db binding.
func isCrossDBBinding(stmt ast.Node) bool {
	for _, t := range CollectTableNames(stmt) {
		if t.Schema.L == "*" {
			return true
		}
	}
	return false
}

// CollectTableNames gets all table names from ast.Node.
// This function is mainly for binding cross-db matching.
// ** the return is read-only.
// For example:
//
//	`select * from t1 where a < 1` --> [t1]
//	`select * from db1.t1, t2 where a < 1` --> [db1.t1, t2]
//
// You can see more example at the TestExtractTableName.
func CollectTableNames(in ast.Node) []*ast.TableName {
	collector := tableNameCollectorPool.Get().(*tableNameCollector)
	defer func() {
		collector.tableNames = nil
		tableNameCollectorPool.Put(collector)
	}()
	in.Accept(collector)
	return collector.tableNames
}

var tableNameCollectorPool = sync.Pool{
	New: func() any {
		return newCollectTableName()
	},
}

type tableNameCollector struct {
	tableNames []*ast.TableName
}

func newCollectTableName() *tableNameCollector {
	return &tableNameCollector{
		tableNames: make([]*ast.TableName, 0, 4),
	}
}

// Enter implements Visitor interface.
func (c *tableNameCollector) Enter(in ast.Node) (out ast.Node, skipChildren bool) {
	if node, ok := in.(*ast.TableName); ok {
		c.tableNames = append(c.tableNames, node)
		return in, true
	}
	return in, false
}

// Leave implements Visitor interface.
func (*tableNameCollector) Leave(in ast.Node) (out ast.Node, ok bool) {
	return in, true
}
