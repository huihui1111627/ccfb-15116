# 容器资源控制组模拟器 (cgroupsim)

竹编非遗工坊传习点小服务器的混跑问题复现工具:直播推流、展陈插件、夜间转码
跑在同一台机器上,用简化内核模型模拟控制组(cgroup)的处理器调度、带宽配额、
内存三档限制与压力停滞指标(PSI),并提供一个看板页面定位"谁在等谁"。

## 运行

```sh
go run ./cmd/cgroupsim -addr :8080 -cpus 4 -seed 42 -speed 8
# 打开 http://localhost:8080
```

页面包含:控制组层级树(权重、配额/周期、本周期 CPU 用量、节流次数与时长)、
每组内存用量(保护线/软上限/硬上限标记)与回收量、OOM 计数、六条压力曲线
(CPU/内存/IO × 部分停滞/全部停滞,10 秒滑动平均),支持整组冻结/解冻。

## 模型要点

- **层级权重**:子组按 `weight` 分父组拿到的份额,需求不足的组先封顶,剩余水填式再分。
- **带宽配额**:按周期+配额限制;配额跑完整组摘下运行队列,下周期开头放回,
  统计节流次数与时长。配额在全局池与每处理器本地池间按小片(默认 5ms)下发,
  空闲小片定时还回全局池。
- **内存三档**:保护线(low)以下回收时跳过;超软上限(high)分配任务被拖慢并
  强制回收缓存;超硬上限(max)先紧急回收,不够则在组内挑匿名页最多的任务杀掉。
- **压力(PSI)**:按任务等处理器/等内存回收/等读写的时间,分部分停滞(some)与
  全部停滞(full);父组按子树全部任务一起算,重叠停滞只算一次;冻结期间不计。
  10s/1m/5m 指数滑动平均按固定间隔(1s)更新,公式与内核一致。
- **触发器**:窗口内累计停滞超阈值边沿触发。

## 测试(统计对账)

```sh
go test ./internal/sim/ -v
```

固定种子生成几十个混合负载,逐项与独立重算结果对账:

| 测试 | 对账内容 |
|---|---|
| TestCPUSharesMatchWeights / Hierarchical | 各组实际 CPU 时间 vs 权重理论份额(±2%) |
| TestQuotaThrottleExact | 节流次数/时长 vs 逐周期手算,并与原始节流记录互算 |
| TestQuotaCapRedistribution | 配额封顶后剩余算力按权重再分 |
| TestSliceReturn | 空闲小片定时还回全局池 |
| TestMemoryHighThrottleAndReclaim / LowProtection / OOMKill | 软上限拖慢回收、保护线免回收、硬上限 OOM;事件数与原始分配记录一致 |
| TestPSIParentUnionNotSum | 父组压力是子树并集而非子组求和 |
| TestPSIFrozenExcluded | 冻结期间压力与 CPU 时间均不累计 |
| TestPSIOfflineRecompute | 全部组×资源×some/full 的累计与三条滑动平均 vs 原始停滞记录离线重算(1e-9 内) |
| TestTriggersExact | 触发器点火序列 vs 离线重算,不漏报不多报 |
