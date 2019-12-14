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

// WindowParallelExec executes `window` in a parallel manner.
// Partitioning, Sorting, and `ConsumeGroupRows` in parallel,
// as what the paper "Efficient Processing of Window Functions in Analytical SQL Queries" says.
// (Need removing `Enforced` child property).
//
//                            +-------------+
//                            | Main Thread |
//                            +------+------+
//                                   ^
//                                   |
//                                   +
//                              +-+-            +-+
//                              | |    ......   | |  finalOutputCh
//                              +++-            +-+
//                               ^
//                               |
//                               +---------------+
//                               |               |
//                 +----------------+           +----------------+
//                 | combine worker |   ......  | combine worker | combine worker: Combining hash groups and Sorting,
//                 +------------+---+           +-+--------------+                 then Evaluating Window Function
//                              ^                 ^
//                              |                 |
//                             +-+  +-+  ......  +-+
//                             | |  | |          | |
//                             ...  ...          ...    partitionOutputChs
//                             | |  | |          | |
//                             +++  +++          +++
//                              ^    ^            ^
//          +-+                 |    |            |
//          | |        +--------o----+            |
// inputCh  +-+        |        +-----------------+---+
//          | |        |                              |
//          ...    +---+--------------+            +--+---------------+
//          | |    | partition worker |   ......   | partition worker | partition worker: Pre-Partitioning into Hash Groups
//          +++    +---------------+--+            +-+----------------+
//           |                     ^                 ^
//           |                     |                 |
//      +----v---------+          +++ +-+           +++
//      | data fetcher | +------> | | | |   ......  | |   partitionInputChs
//      +--------------+          +-+ +-+           +-+
//
////////////////////////////////////////////////////////////////////////////////////////
type WindowParallelExec struct {
}
