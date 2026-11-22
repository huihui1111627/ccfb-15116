package sim

// allocItem 是参与整数分配的一个候选 (任务或组)。
type allocItem struct {
	weight int64 // 权重; 非正权重按 1 处理
	cap    int64 // 本 tick 能消耗的上限 (ms·CPU); 0 表示无上限
	got    int64 // 输出: 实际分到的整数毫秒
}

// largestRemainder 用最大余数法 (Hamilton 方法) 把 total 个整数单位
// 按权重在 items 间分配, 每个 item 不超过自己的 cap。
//
// 先按权重取整分配 floor (受 cap 约束), 再把剩余单位逐个分给小数部分
// 最大的 item; 并列时按下标稳定决胜, 保证固定种子下逐 tick 可复现、
// 可手算。返回实际分出去的总量 (可能小于 total, 例如所有 item 都被
// cap 卡住)。
func largestRemainder(items []allocItem, total int64) int64 {
	n := len(items)
	if n == 0 || total <= 0 {
		return 0
	}

	var wsum int64
	for i := range items {
		w := items[i].weight
		if w <= 0 {
			w = 1
		}
		items[i].weight = w
		items[i].got = 0
		wsum += w
	}

	// 第一步: 按比例取整, 同时尊重 cap。cap 截断产生的余量留给第二步。
	var given int64
	for i := range items {
		raw := items[i].weight * total / wsum
		got := raw
		if c := items[i].cap; c > 0 && got > c {
			got = c
		}
		if got > total-given {
			got = total - given
		}
		items[i].got = got
		given += got
	}

	// 第二步: 剩余单位按小数部分大小逐个补发。
	// 小数部分用整数分子 r_i = w_i*total - got_i*wsum 比较 (分母都是
	// wsum); 已触顶的候选跳过, 并列时保留先出现的下标 (稳定)。
	for remaining := total - given; remaining > 0; remaining-- {
		best := -1
		var bestNum int64
		for i := range items {
			if c := items[i].cap; c > 0 && items[i].got >= c {
				continue
			}
			num := items[i].weight*total - items[i].got*wsum
			if best == -1 || num > bestNum {
				best, bestNum = i, num
			}
		}
		if best == -1 {
			break // 全部触顶
		}
		items[best].got++
	}

	given = 0
	for i := range items {
		given += items[i].got
	}
	return given
}
