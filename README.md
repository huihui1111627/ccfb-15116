# cgsim · 控制组资源控制模拟器

竹编非遗工坊传习点的小服务器上, 直播推流、展陈插件、夜间转码混跑在
同一台机器, 转码一启动直播就卡, 但只看 CPU 使用率看不出谁在等谁。
本项目用 Go 在用户态模拟一棵 cgroup v2 风格的控制组树, 把内核的
**CPU 权重/带宽调度、配额分片、内存三档回收/OOM、层级 PSI 压力、
冻结器** 全部用确定性的离散 tick 模型复现, 并带一个实时看板:

- 控制组层级树 (含节流/冻结状态、整组冻结/解冻)
- 每个组的处理器配额、实际用量、本地池与节流统计
- 内存保护线/软上限/硬上限三档用量与回收、OOM 统计
- 每个组 CPU/内存/IO 三类资源的 some/full 压力曲线 (10s/1m/5m EWMA)

整个引擎不依赖宿主机 cgroup、不依赖墙钟, 同一种子逐 tick 完全可复现。

## 运行

```bash
# 看板模式 (默认 :8080, 后台自动推进模拟)
go run ./cmd/cgsim
# 打开 http://localhost:8080

# 自定义: 种子 / tick 数 / CPU 数 / 推进速度
go run ./cmd/cgsim -seed 20240521 -ticks 30000 -cpus 4 -step-ms 50

# 无界面模式: 跑完打印每组汇总, 用于脚本核对
go run ./cmd/cgsim -headless -ticks 8000 -seed 7919
```

看板支持暂停/单步/调速, 点击层级树切换选中组, 右上角按钮可把任意组
整组冻结或解冻 (冻结期间该子树不参与调度, 也不计压力)。

## 模型语义 (与需求逐条对应)

时间单位: 1 tick = 1 模拟毫秒; CPU 时间以 ms·CPU 计; 内存以字节计。

1. **层级权重 (需求 1)** — 容量自根向下逐层分配, 同层实体 (子组与
   直属任务) 用 stride/虚拟运行时间调度: 每发放 1ms, 选中者的
   `vRuntime += 本层权重和 / 自己权重`, 长期占比严格收敛到权重比。
   睡眠唤醒时 vRuntime 对齐同层最小值, 防止睡醒后补领。
2. **周期配额与节流 (需求 2)** — 组按 `Period/Quota` 限带宽, 周期内
   配额耗尽且仍有需求时整组节流, 组内所有任务摘下运行队列, 直到下一
   个周期开头放回; 摘下次数 (`throttle_cnt`) 与墙钟时长
   (`throttle_ms`) 逐 tick 记账。
3. **配额分片与归还 (需求 3)** — 配额先留在组全局池, 处理器本地池
   耗尽时按 `Slice` 借一个分片; 本地池静默超过 `ReturnAfter` 个 tick
   未使用就把余额归还全局池。
4. **内存三档 (需求 4)** — `MemLow` 保护线 (回收不压到线以下)、
   `MemSoft` 软上限 (超限分配进入 memWait, 任务被拖慢并每 tick 强制
   回收)、`MemHard` 硬上限 (回收仍放不下时在组子树内挑内存最大的
   任务杀掉)。文件类内存直接回收, 匿名内存按比例压缩。
5. **压力口径 (需求 5)** — 逐任务按等待 CPU / 等待内存回收 / 等待
   IO 记停滞, 分 some (至少一个活跃任务停滞) 与 full (全部活跃任务
   停滞) 两档; 没有活跃任务的 tick 不算压力。
6. **三窗口 EWMA (需求 6)** — 按固定 `flush` 间隔更新 10s/1m/5m
   三个指数滑动平均, 衰减系数 `exp(-flush/window)`, 与 Linux PSI
   同款; 看板按 flush 点画曲线。
7. **层级去重 (需求 7)** — 父组压力每次重新扫描整棵子树的任务做
   布尔聚合, **绝不把子组数值相加**; 同一 tick 多个子组同时停滞,
   父组只记一次。
8. **冻结器 (需求 8)** — 组 (含祖先) 冻结后子树任务不参与调度,
   该 tick 分子分母都不进压力统计; 节流墙钟仍照走。

## 测试如何证明正确性

测试全部在 `internal/sim` 与 `internal/web` 包内, `go test ./...`
一键运行 (带 `-race` 已通过, sim 包覆盖率约 90%)。

| 需求 | 测试 | 校验方式 |
|---|---|---|
| 权重份额 | `TestCPUWeightedShare`, `...MultiCPU` | 容量守恒 + 长期占比收敛到权重比 (1:3、1:1:2) |
| 整数分配 | `TestLargestRemainder*` | 最大余数法守恒、cap 截断、与理想份额偏差 ≤1 |
| 节流次数/时长 | `TestCPUThrottleHandCalc`, `TestThrottleHandCalcAcrossSeeds` | 逐周期手算事件 tick、次数、时长、用量, 12 个种子全核对 |
| 配额=周期边界 | `TestCPUQuotaEqualsPeriodNoThrottle` | 耗尽点与周期边界重合时零节流 (与真实内核一致) |
| 分片借还 | `TestCPUSlicePoolBorrowReturn` | 逐次核对全局池/本地池余额与归还 |
| 配额切断权重 | `TestCPUQuotaCappedShare` | 小配额组的富余容量必须让给同层组 |
| 内存三档 | `TestMemorySoftLimitReclaim`, `...HardLimitOOM`, `...LowProtection` | 守恒关系、回收事件、OOM 牺牲者与内存释放、保护线不被击穿 |
| EWMA 平均值 | `TestPSIvsOfflineRecomputation` | 独立离线实现消费逐 tick 原始停滞记录重算, 与引擎逐点逐窗口比对, 误差 1e-9 内 |
| 层级去重 | `TestPSIParentNoDoubleCount` | 4 任务抢 1 CPU 时父组 some 恒为 100% 而非 200% |
| 冻结剔除 | `TestPSIFreezeExcluded` | 冻结 tick 不进原始记录/EWMA, 离线重算一致 |
| some/full 语义 | `TestPSIFullVsSome` | 一跑一等时 some 恒真、full 恒假 |
| 触发器不漏不多报 | `TestTriggerAudit` + `validateSweep` | 节流/解节流、冻结/解冻严格交替, OOM 必有死任务, 回收事件字节为正, 计数与日志一致 |
| 固定种子混合负载 | `TestMixedLoadSweep` (36 个种子)、`TestDeterminism` | 同种子两次运行逐字段一致; 各种子全套不变量 (容量守恒、配额不超发、内存守恒、EWMA 离线一致、触发器合法) |
| Web 接口 | `internal/web/server_test.go` | 首页、快照、冻结/解冻、单步、SSE 推流 |

## 目录结构

```
cmd/cgsim/main.go          入口: 看板模式 / headless 汇总
internal/sim/
  types.go                 组/任务/事件等运行时类型
  rng.go                   自带确定性 LCG (不依赖 math/rand 版本行为)
  alloc.go                 最大余数法整数分配
  cpu.go                   stride 调度、周期节流、全局/本地配额池
  memory.go                三档阈值、回收、OOM 牺牲者选择
  psi.go                   逐 tick 停滞聚合、三窗口 EWMA、历史曲线
  pressure.go              子树扫描与层级去重
  events.go                脚本事件与冻结控制
  engine.go                场景校验、层级构建、tick 主循环
  generate.go              固定种子混合负载生成器
  snapshot.go              看板序列化视图与控制接口
internal/web/
  server.go                HTTP/SSE/冻结/单步接口
  index.go                 自包含单文件看板 (原生 JS + Canvas)
```

## 设计取舍

- **整数 tick 调度**: 所有调度决策以 1ms 为最小单位, 不用浮点做配额
  扣减, 保证逐周期可手算; 仅 stride 的虚拟运行时间用浮点, 但同输入
  运算顺序固定, 结果可复现。
- **回收是真释放**: 软上限等待期回收的文件内存会真实丢弃, 因此等待
  结束后落账量由 "申请量 − 回收量" 守恒决定, 而不是无条件满足申请。
- **空 flush 桶跳过**: 整段冻结的区间不产出 PSI 点, EWMA 保持原值,
  与离线重算的墙钟分桶口径一致。
