package sim

// rng 是确定性的伪随机数源。
// 测试要求固定种子完全可复现, 且不能依赖 math/rand 在不同 Go 版本间
// 可能变化的算法, 因此这里自带一个 64 位 LCG (Knuth MMIX 常数)。
type rng struct {
	state uint64
}

func newRNG(seed uint64) *rng {
	// 零状态会让 LCG 退化为固定序列, 先做一次混合避免常见的 seed=0。
	return &rng{state: seed*0x9E3779B97F4A7C15 + 0xD1B54A32D192ED03}
}

// uint64 返回一个伪随机 64 位无符号整数。
func (r *rng) uint64() uint64 {
	r.state = r.state*6364136223846793005 + 1442695040888963407
	return r.state
}

// intn 返回 [0, n) 区间内的整数, n 必须为正。
func (r *rng) intn(n int) int {
	if n <= 0 {
		panic("intn: non-positive bound")
	}
	return int(r.uint64() % uint64(n))
}

// intBetween 返回 [lo, hi] 闭区间内的整数。
func (r *rng) intBetween(lo, hi int) int {
	if hi < lo {
		panic("intBetween: empty range")
	}
	return lo + r.intn(hi-lo+1)
}

// float64 返回 [0, 1) 区间内的浮点数。
func (r *rng) float64() float64 {
	// 取高 53 位保证尾数精度。
	return float64(r.uint64()>>11) / float64(1<<53)
}

// chance 以概率 p 返回 true。
func (r *rng) chance(p float64) bool {
	return r.float64() < p
}

// choice 从整数切片中等概率挑一个元素。
func (r *rng) choice(xs []int) int {
	return xs[r.intn(len(xs))]
}
