package sim

import (
	"math"
	"testing"
)

func TestLargestRemainderBasic(t *testing.T) {
	// 权重 1:2:3 分 6 个单位 -> 精确得 1:2:3。
	items := []allocItem{
		{weight: 1}, {weight: 2}, {weight: 3},
	}
	given := largestRemainder(items, 6)
	if given != 6 {
		t.Fatalf("given=%d want 6", given)
	}
	want := []int64{1, 2, 3}
	for i := range want {
		if items[i].got != want[i] {
			t.Fatalf("item %d got=%d want=%d", i, items[i].got, want[i])
		}
	}
}

func TestLargestRemainderRounding(t *testing.T) {
	// 权重 1:1:1 分 10 -> 4:3:3, 多出来的 1 个给下标最小者 (稳定决胜)。
	items := []allocItem{{weight: 1}, {weight: 1}, {weight: 1}}
	largestRemainder(items, 10)
	want := []int64{4, 3, 3}
	for i := range want {
		if items[i].got != want[i] {
			t.Fatalf("item %d got=%d want=%d", i, items[i].got, want[i])
		}
	}
}

func TestLargestRemainderNoLoss(t *testing.T) {
	// 任意权重组合下, 分配总量守恒且不超需求。
	r := newRNG(42)
	for trial := 0; trial < 200; trial++ {
		n := r.intBetween(1, 8)
		items := make([]allocItem, n)
		var wsum int64
		for i := range items {
			items[i] = allocItem{weight: int64(r.intBetween(1, 500))}
			wsum += items[i].weight
		}
		total := int64(r.intBetween(1, 100))
		given := largestRemainder(items, total)
		if given > total {
			t.Fatalf("trial %d: given %d > total %d", trial, given, total)
		}
		var sum int64
		for _, it := range items {
			sum += it.got
		}
		if sum != given {
			t.Fatalf("trial %d: sum %d != given %d", trial, sum, given)
		}
		// 无 cap 时必须全部分完。
		if given != total {
			t.Fatalf("trial %d: uncapped allocation %d != total %d (wsum=%d)", trial, given, total, wsum)
		}
		// 按权重的理论份额与实际差不超过 1 (Hamilton 方法保证)。
		for _, it := range items {
			ideal := float64(it.weight) * float64(total) / float64(wsum)
			if math.Abs(float64(it.got)-ideal) > 1.0+1e-9 {
				t.Fatalf("trial %d: got %d deviates from ideal %.3f by >1", trial, it.got, ideal)
			}
		}
	}
}

func TestLargestRemainderCaps(t *testing.T) {
	// 权重 1:1:1 分 9, 但前两个各被 cap 在 2 -> 2:2:5。
	items := []allocItem{
		{weight: 1, cap: 2}, {weight: 1, cap: 2}, {weight: 1, cap: 0},
	}
	given := largestRemainder(items, 9)
	want := []int64{2, 2, 5}
	var sum int64
	for i := range want {
		if items[i].got != want[i] {
			t.Fatalf("item %d got=%d want=%d", i, items[i].got, want[i])
		}
		sum += items[i].got
	}
	if sum != given {
		t.Fatalf("sum=%d given=%d", sum, given)
	}

	// 全部触顶: 分 100 但总 cap 只有 6 -> 实际给出 6。
	items = []allocItem{
		{weight: 1, cap: 2}, {weight: 1, cap: 2}, {weight: 1, cap: 2},
	}
	given = largestRemainder(items, 100)
	if given != 6 {
		t.Fatalf("given=%d want 6", given)
	}
}

func TestLargestRemainderCapsExtra(t *testing.T) {
	// 全部触顶: 分 100 但总 cap 只有 6 -> 实际给出 6。
	items := []allocItem{
		{weight: 1, cap: 2}, {weight: 1, cap: 2}, {weight: 1, cap: 2},
	}
	given := largestRemainder(items, 100)
	if given != 6 {
		t.Fatalf("given=%d want 6", given)
	}
}
