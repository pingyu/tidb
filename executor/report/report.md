### 当前进展：完成功能开发和测试，make dev通过

### 目前问题：性能太差 T_T



### Benchmark

4线程并行比串行慢20倍。
（94771954 ns/op vs. 4963490 ns/op，func:row_number, ndv:1000, rows:100000）
（MacbookPro，CPU 6 cores，Mem 16GB。下同）

```
$ go test -v -benchmem -bench=BenchmarkWindowRows -run=BenchmarkWindowRows 
goos: darwin
goarch: amd64
pkg: github.com/pingcap/tidb/executor
BenchmarkWindowRows/(func:row_number,_ndv:10,_rows:1000,_concurrency:1)-12         	   22903	     45968 ns/op	   82136 B/op	     159 allocs/op
BenchmarkWindowRows/(func:row_number,_ndv:1000,_rows:1000,_concurrency:1)-12       	   10000	    106774 ns/op	   81688 B/op	    1062 allocs/op
BenchmarkWindowRows/(func:row_number,_ndv:10,_rows:100000,_concurrency:1)-12       	     236	   5007531 ns/op	12068424 B/op	    5066 allocs/op
BenchmarkWindowRows/(func:row_number,_ndv:1000,_rows:100000,_concurrency:1)-12     	     246	   4963490 ns/op	 7905256 B/op	   13029 allocs/op
BenchmarkWindowRows/(func:row_number,_ndv:10,_rows:1000,_concurrency:4)-12         	     350	   5674948 ns/op	28061750 B/op	     476 allocs/op
BenchmarkWindowRows/(func:row_number,_ndv:1000,_rows:1000,_concurrency:4)-12       	     366	   3498528 ns/op	27111568 B/op	    1884 allocs/op
BenchmarkWindowRows/(func:row_number,_ndv:10,_rows:100000,_concurrency:4)-12       	      16	  78302655 ns/op	153143149 B/op	    3620 allocs/op
BenchmarkWindowRows/(func:row_number,_ndv:1000,_rows:100000,_concurrency:4)-12     	      16	  94771954 ns/op	148842062 B/op	   11961 allocs/op
PASS
ok  	github.com/pingcap/tidb/executor	92.151s
```



### 分析结论

初步分析，主要原因包括：

1. **线程间同步**开销太大。pprof结果显示`runtime.pthread_cond_signal`占了大部分CPU。
2. **内存拷贝**开销。pprof结果中`runtime.memmove`也是大头，主要由于`(*windowGroupingWorker).copyOtherFields`中的字段拷贝导致。而串行版本中 `(*WindowExec).copyChk`的性能非常高。
3. 每个线程**计算量较小**，核心逻辑只是group计算。导致并行化带来的提升不大。

pprof结果（详见 cpu-4.prof文件）
```
$ go test -v -benchmem -cpuprofile cpu-4.prof -memprofile mem-4.prof -bench=BenchmarkWindowRows -run=BenchmarkWindowRows
goos: darwin
goarch: amd64
pkg: github.com/pingcap/tidb/executor
BenchmarkWindowRows/(func:row_number,_ndv:1000,_rows:100000,_concurrency:4)-12         	      15	  69813126 ns/op	148993537 B/op	   12423 allocs/op
PASS
ok  	github.com/pingcap/tidb/executor	3.951s
```
```
$ go tool pprof -text report/cpu-4.prof 
Type: cpu
Time: Dec 17, 2019 at 11:42pm (CST)
Duration: 3.79s, Total samples = 7.37s (194.35%)
Showing nodes accounting for 7.10s, 96.34% of 7.37s total
Dropped 72 nodes (cum <= 0.04s)
      flat  flat%   sum%        cum   cum%
     3.34s 45.32% 45.32%      3.34s 45.32%  runtime.pthread_cond_signal
     2.57s 34.87% 80.19%      2.57s 34.87%  runtime.memmove
     0.52s  7.06% 87.25%      0.52s  7.06%  runtime.pthread_cond_wait
     0.17s  2.31% 89.55%      0.17s  2.31%  runtime.memclrNoHeapPointers
     0.16s  2.17% 91.72%      0.16s  2.17%  runtime.usleep
     0.05s  0.68% 92.40%      0.05s  0.68%  github.com/pingcap/tidb/util/chunk.(*Column).finishAppendVar
     0.05s  0.68% 93.08%      0.05s  0.68%  runtime.madvise
     0.05s  0.68% 93.76%      0.05s  0.68%  runtime.pthread_cond_timedwait_relative_np
     0.04s  0.54% 94.30%      0.06s  0.81%  github.com/pingcap/tidb/util/chunk.Row.GetDatum
     0.04s  0.54% 94.84%      0.04s  0.54%  runtime.(*waitq).dequeueSudoG
     0.02s  0.27% 95.12%      0.81s 10.99%  github.com/pingcap/tidb/executor.buildMockDataSource
     0.02s  0.27% 95.39%      0.13s  1.76%  runtime.gcDrain
     0.02s  0.27% 95.66%      0.10s  1.36%  runtime.scanobject
     0.01s  0.14% 95.79%      0.07s  0.95%  github.com/pingcap/tidb/executor.(*mockDataSource).genColDatums
     0.01s  0.14% 95.93%      1.29s 17.50%  github.com/pingcap/tidb/util/chunk.(*Chunk).CopyConstruct
     0.01s  0.14% 96.07%      0.85s 11.53%  github.com/pingcap/tidb/util/chunk.(*Column).AppendBytes
     0.01s  0.14% 96.20%      0.04s  0.54%  runtime.greyobject
     0.01s  0.14% 96.34%      0.13s  1.76%  runtime.mallocgc
...
```



### 解决方案：将排序放在worker线程内进行

理由包括：
1. 可以增加worker线程的计算量。
2. worker内排序后的Chunk与结果Chunk一一对应，从而可以仿照串行版本copyChk的实现方式，大大减少内存拷贝。
3. 论文《Efficient Processing of Window Functions in Analytical SQL Queries》就是这么说的 [奸笑]

此外，串行 vs. 并行，实际上是 `Sort + StreamSequentialWindow` vs. `HashParallelWindow`。单纯的 `StreamSequentialWindow` vs. `HashParallelWindow` 可能不分仲伯（参考了`BenchmarkAggConcurrency`的结果。除非`Framing`巨复杂？这块还没有验证过）。所以，physical plan做cost-based选择的时候，可能要把这块考虑进去。如果下层算子的结果已经是按照partitonBy + orderBy有序，串行的`StreamSequentialWindow`可能是更好的方案。





### 附录
BenchmarkAggConcurrency结果
```
$ go test -v -benchmem -bench=BenchmarkAggConcurrency -run=BenchmarkAggConcurrency
goos: darwin
goarch: amd64
pkg: github.com/pingcap/tidb/executor
BenchmarkAggConcurrency/(execType:hash,_aggFunc:sum,_ndv:1000,_hasDistinct:false,_rows:10000000,_concurrency:1)-12         	       1	1609973579 ns/op	320491648 B/op	20004175 allocs/op
BenchmarkAggConcurrency/(execType:stream,_aggFunc:sum,_ndv:1000,_hasDistinct:false,_rows:10000000,_concurrency:1)-12       	       8	 145164439 ns/op	   69848 B/op	      48 allocs/op
BenchmarkAggConcurrency/(execType:hash,_aggFunc:sum,_ndv:1000,_hasDistinct:false,_rows:10000000,_concurrency:4)-12         	       5	 200148967 ns/op	242554860 B/op	   58095 allocs/op
BenchmarkAggConcurrency/(execType:hash,_aggFunc:sum,_ndv:1000,_hasDistinct:false,_rows:10000000,_concurrency:8)-12         	       8	 138973642 ns/op	244335376 B/op	   82912 allocs/op
BenchmarkAggConcurrency/(execType:hash,_aggFunc:sum,_ndv:1000,_hasDistinct:false,_rows:10000000,_concurrency:15)-12        	       9	 123728173 ns/op	247432652 B/op	  126553 allocs/op
BenchmarkAggConcurrency/(execType:hash,_aggFunc:sum,_ndv:1000,_hasDistinct:false,_rows:10000000,_concurrency:20)-12        	       9	 122966765 ns/op	249714496 B/op	  157897 allocs/op
BenchmarkAggConcurrency/(execType:hash,_aggFunc:sum,_ndv:1000,_hasDistinct:false,_rows:10000000,_concurrency:30)-12        	       8	 130866201 ns/op	254129238 B/op	  221007 allocs/op
BenchmarkAggConcurrency/(execType:hash,_aggFunc:sum,_ndv:1000,_hasDistinct:false,_rows:10000000,_concurrency:40)-12        	       8	 128582851 ns/op	258561764 B/op	  284807 allocs/op
PASS
ok  	github.com/pingcap/tidb/executor	32.503s
```

