# High Performance TiDB

## Week 1

### PD
Build and run `PD` followig [README.md](https://github.com/pingcap/pd/blob/master/README.md):
```sh
git clone https://github.com/pingcap/pd.git
cd pd
make

export DATA=~/workspace/data
bin/pd-server --name="pd" --data-dir="${DATA}/pd" --log-file=${DATA}/pd.log &
```

### TiKV
Build and run `TiKV` following [CONTRIBUTING.md](https://github.com/tikv/tikv/blob/master/CONTRIBUTING.md):
```sh
git clone https://github.com/tikv/tikv.git
cd tikv
make build

export TIKV="./target/debug/tikv-server"
export DATA="~/workspace/data"

${TIKV} --pd-endpoints="127.0.0.1:2379" \
                --addr="127.0.0.1:20160" \
                --data-dir=${DATA}/tikv1 \
                --log-file=${DATA}/tikv1.log &

${TIKV} --pd-endpoints="127.0.0.1:2379" \
                --addr="127.0.0.1:20161" \
                --data-dir=${DATA}/tikv2 \
                --log-file=${DATA}/tikv2.log &

${TIKV} --pd-endpoints="127.0.0.1:2379" \
                --addr="127.0.0.1:20162" \
                --data-dir=${DATA}/tikv3 \
                --log-file=${DATA}/tikv3.log &
```

### TiDB
Build `TiDB` following [README.md](https://github.com/pingcap/community/blob/master/contributors/README.md#tidb):
```sh
git clone https://github.com/pingcap/tidb.git
cd tidb
make server
```

Modify related transaction [codes](https://github.com/pingcap/tidb/compare/master...pingyu:high_performance_tidb), build again, and run:
```sh
make server
make dev
bin/tidb-server -config config/my-config.toml
```
config/my-config.toml
```toml
# Registered store name, [tikv, mocktikv]
store = "tikv"

# TiDB storage path.
path = "127.0.0.1:2379"
```

### Manual Test
Run `Insert`:
```sql
mysql> insert into t1 values (100,100,100);
Query OK, 1 row affected (0.01 sec)
```

The following was printed on console of `TiDB`:
```sh
[2020/08/16 21:42:52.342 +08:00] [INFO] [kv.go:282] ["hello transaction"]
[2020/08/16 21:42:52.346 +08:00] [WARN] [2pc.go:1006] ["schemaLeaseChecker is not set for this transaction, schema check skipped"] [connID=0] [startTS=418797419838832642] [commitTS=418797419838832643]
```
