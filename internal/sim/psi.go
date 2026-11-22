package sim

import "math"

// 资源下标, 全模块统一使用。
const (
	ResCPU = 0
	ResMem = 1
	ResIO  = 2
)

var resNames = [...]string{"cpu", "memory", "io"}

// PSIPoint 是一次 flush 时刻的压力快照, 供看板画曲线。
type PSIPoint struct {
	Tick    int64         `json:"tick"`
	Some    [3]float64    `json:"some"`     // 本区间各资源部分停滞占比 [0,1]
	Full    [3]float64    `json:"full"`     // 本区间各资源全部停滞占比 [0,1]
	AvgSome [3][3]float64 `json:"avg_some"` // [资源][窗口(10s/1m/5m)] 百分比
	AvgFull [3][3]float64 `json:"avg_full"`
}

// rawTick 保存每个 tick 的原始停滞判定, 测试用它离线重算 EWMA。
type rawTick struct {
	tick   int64
	frozen bool
	some   [3]bool
	full   [3]bool
}

// psiState 持有一个组的全部压力记账。父组与子组各自独立持有,
// 父组的观测值由引擎汇总整棵子树的任务后喂入 (不做子组数值相加)。
type psiState struct {
	windows [3]int64 // 三个 EWMA 窗口长度 (tick)
	flush   int64    // EWMA 更新间隔 (tick)
	decays  [3]float64

	// 当前 flush 区间的累计。
	intervalTicks int64 // 区间内非冻结 tick 数 (EMA 的分母)
	someTicks     [3]int64
	fullTicks     [3]int64

	// EWMA 当前值, 百分比口径 [0,100]。
	AvgSome [3][3]float64
	AvgFull [3][3]float64

	// 全生命周期累计停滞 tick 数。
	TotalSome [3]int64
	TotalFull [3]int64

	history []PSIPoint
	raw     []rawTick
	record  bool
}

func newPSI(windows [3]int64, flush int64, record bool) *psiState {
	p := &psiState{
		windows: windows,
		flush:   flush,
		record:  record,
	}
	for k, w := range windows {
		if w <= 0 {
			panic("psi: window must be positive")
		}
		// Linux PSI 同款衰减: 每个 flush 区间按 exp(-flush/window) 衰减。
		p.decays[k] = math.Exp(-float64(flush) / float64(w))
	}
	return p
}

// observe 喂入一个 tick 的停滞判定。frozen 为 true 时该 tick 完全不计:
// 既不进分子也不进分母 (冻结期间不算压力)。
func (p *psiState) observe(tick int64, frozen bool, some, full [3]bool) {
	if p.record {
		p.raw = append(p.raw, rawTick{tick: tick, frozen: frozen, some: some, full: full})
	}
	if frozen {
		return
	}
	p.intervalTicks++
	for r := 0; r < 3; r++ {
		if some[r] {
			p.someTicks[r]++
			p.TotalSome[r]++
		}
		if full[r] {
			p.fullTicks[r]++
			p.TotalFull[r]++
		}
	}
}

// maybeFlush 在引擎每个 tick 末尾调用; 到达 flush 边界 (含模拟结束时的
// 收尾 partial flush) 时更新 EWMA 并写一个历史点。
// 边界约定: 观测 tick 0..flush-1 后, 在 tick=flush-1 末尾产出第一个点,
// 即条件 (tick+1) 是 flush 的整数倍。整段冻结 (区间内没有任何非冻结
// 观测) 的桶不产出点, EWMA 保持原值。
func (p *psiState) maybeFlush(tick int64, force bool) {
	atBoundary := (tick+1)%p.flush == 0
	if !force && !atBoundary {
		return
	}
	if force && atBoundary {
		return // 正常边界已处理, 收尾不重复
	}
	if p.intervalTicks == 0 {
		return // 空桶 (整段冻结或无观测)
	}
	p.emit(tick + 1)
}

func (p *psiState) emit(tick int64) {
	den := float64(p.intervalTicks)
	var pt PSIPoint
	pt.Tick = tick
	for r := 0; r < 3; r++ {
		if p.intervalTicks > 0 {
			pt.Some[r] = float64(p.someTicks[r]) / den
			pt.Full[r] = float64(p.fullTicks[r]) / den
		}
		for k := 0; k < 3; k++ {
			d := p.decays[k]
			if p.intervalTicks > 0 {
				pt.AvgSome[r][k] = p.AvgSome[r][k]*d + pt.Some[r]*100*(1-d)
				pt.AvgFull[r][k] = p.AvgFull[r][k]*d + pt.Full[r]*100*(1-d)
			} else {
				pt.AvgSome[r][k] = p.AvgSome[r][k]
				pt.AvgFull[r][k] = p.AvgFull[r][k]
			}
		}
	}
	p.AvgSome = pt.AvgSome
	p.AvgFull = pt.AvgFull
	p.history = append(p.history, pt)
	p.intervalTicks = 0
	p.someTicks = [3]int64{}
	p.fullTicks = [3]int64{}
}

// historyCopy 返回历史曲线的拷贝, 最多保留 max 个点; 无数据时返回空切片。
func (p *psiState) historyCopy(max int) []PSIPoint {
	if len(p.history) == 0 {
		return []PSIPoint{}
	}
	if len(p.history) > max && max > 0 {
		return append([]PSIPoint(nil), p.history[len(p.history)-max:]...)
	}
	return append([]PSIPoint(nil), p.history...)
}
