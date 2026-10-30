package money

import "testing"

func TestApplyRateRounding(t *testing.T) {
	cases := []struct {
		base Money
		bps  int64
		want Money
	}{
		{FromYuan(100, 0), 250, FromYuan(2, 50)},
		{FromYuan(0, 1), 5000, FromYuan(0, 1)}, // 0.5 分 -> 1 分
		{FromYuan(0, 1), 4999, Money(0)},       // 0.4999 分 -> 0
		{FromYuan(-100, 0), 250, Money(-250)},  // 负数对称：-100.00 * 2.5% = -2.50
	}
	for _, c := range cases {
		if got := ApplyRate(c.base, c.bps); got != c.want {
			t.Fatalf("ApplyRate(%s,%d)=%s want %s", c.base, c.bps, got, c.want)
		}
	}
}

func TestAllocateZeroSum(t *testing.T) {
	// 池子必须分到零：100.00 / 3 人
	got := Allocate(FromYuan(100, 0), []int64{1, 1, 1})
	if Sum(got...) != FromYuan(100, 0) {
		t.Fatalf("sum=%s", Sum(got...))
	}
	want := []Money{FromYuan(33, 34), FromYuan(33, 33), FromYuan(33, 33)}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("largest remainder mismatch: got %v want %v", got, want)
		}
	}
}

func TestAllocateWeighted(t *testing.T) {
	got := Allocate(Money(1001), []int64{60, 30, 10})
	if Sum(got...) != 1001 {
		t.Fatalf("sum=%d want 1001", Sum(got...))
	}
	if got[0] <= got[1] || got[1] <= got[2] {
		t.Fatalf("权重顺序未被尊重: %v", got)
	}
}

// 权重量级远大于总额时（池分发以"分为权重"的常见情形），仍必须按权重比例拆分。
func TestAllocateLargeWeights(t *testing.T) {
	got := Allocate(FromYuan(44460, 0), []int64{828000, 327000, 80000})
	if Sum(got...) != FromYuan(44460, 0) {
		t.Fatalf("sum=%s", Sum(got...))
	}
	// 6:3:1 左右的分布，第一名必须明显领先，不能退化成均分。
	if !(got[0] > got[1] && got[1] > got[2]) {
		t.Fatalf("权重未被尊重: %v", got)
	}
	if got[0] < FromYuan(29000, 0) || got[0] > FromYuan(30500, 0) {
		t.Fatalf("最大份额偏差过大: %s", got[0])
	}
}

func TestAllocateZeroWeightFallsBackToEqual(t *testing.T) {
	got := Allocate(Money(10), []int64{0, 0})
	if got[0] != 5 || got[1] != 5 {
		t.Fatalf("got %v", got)
	}
}
