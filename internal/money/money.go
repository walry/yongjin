// Package money 提供定点金额运算。金额一律以最小货币单位（分）为 int64，
// 禁止出现 float64，避免大额订单上的累积误差。
package money

import (
	"fmt"
	"math/big"
	"sort"
)

// Money 是以"分"为单位的金额。
type Money int64

// BPS 是万分比基数：10000 bps = 100%。
const BPS int64 = 10000

func (m Money) String() string {
	v := int64(m)
	sign := ""
	if v < 0 {
		sign = "-"
		v = -v
	}
	return fmt.Sprintf("%s%d.%02d", sign, v/100, v%100)
}

// FromYuan 便于测试与示例数据构造：FromYuan(8000, 50) = 8000.50 元。
func FromYuan(yuan, cents int64) Money { return Money(yuan*100 + cents) }

// ApplyRate 返回 base * bps / 10000，四舍五入（远离零）。
func ApplyRate(base Money, bps int64) Money { return Money(mulDivRound(int64(base), bps, BPS)) }

func mulDivRound(a, b, d int64) int64 {
	if d == 0 {
		panic("money: division by zero")
	}
	neg := (a < 0) != (b < 0)
	aa, bb := a, b
	if aa < 0 {
		aa = -aa
	}
	if bb < 0 {
		bb = -bb
	}
	q, r := aa/d, aa%d
	// a*b/d == q*b + r*b/d，先拆开可避免大额相乘溢出。
	res := q*bb + (2*r*bb+d)/(2*d)
	if neg {
		return -res
	}
	return res
}

// ApplyChain 依次施加多个乘数，**只在最后舍入一次**（远离零）。
//
// 倍率模型下应得 = 基数 × 基准费率 × 档位倍数 × 时间 × 产品 × 区域，
// 若每步都调 ApplyRate，四次舍入在 800 万级订单上会漂几分到几毛；
// 若先把各 bps 相乘，int64 会溢出（1e8 分 × 1e4⁴ = 1e24）。
// 因此中间用 big.Int 分子分母累乘，末尾统一四舍五入。
//
// 单个乘数时结果必须与 ApplyRate 完全一致（TestApplyChainMatchesApplyRate 守这条）。
func ApplyChain(base Money, bps ...int64) Money {
	if len(bps) == 0 {
		return base
	}
	num := big.NewInt(int64(base))
	den := big.NewInt(1)
	for _, b := range bps {
		num.Mul(num, big.NewInt(b))
		den.Mul(den, big.NewInt(BPS))
	}
	// 四舍五入远离零：|num|*2 + den) / (2*den)
	neg := num.Sign() < 0
	num.Abs(num)
	num.Mul(num, big.NewInt(2))
	num.Add(num, den)
	den.Mul(den, big.NewInt(2))
	q := new(big.Int).Quo(num, den)
	if neg {
		q.Neg(q)
	}
	return Money(q.Int64())
}

// Allocate 按权重把 total 精确拆分，使用最大余数法，保证 Sum(out) == total。
// 权重全为 0 时退化为均分。负数金额同样成立。
func Allocate(total Money, weights []int64) []Money {
	n := len(weights)
	out := make([]Money, n)
	if n == 0 {
		return out
	}
	w := make([]int64, n)
	var sum int64
	for i, x := range weights {
		if x > 0 {
			w[i] = x
		}
		sum += w[i]
	}
	if sum == 0 {
		for i := range w {
			w[i] = 1
		}
		sum = int64(n)
	}

	neg := total < 0
	amt := int64(total)
	if neg {
		amt = -amt
	}

	// amt*w/sum 拆成 q*w + (r*w)/sum，避免大权重下整数部分被丢弃。
	q, r := amt/sum, amt%sum
	type frac struct {
		idx int
		rem int64
	}
	fracs := make([]frac, n)
	var assigned int64
	for i := range w {
		part := r * w[i]
		base := q*w[i] + part/sum
		out[i] = Money(base)
		fracs[i] = frac{idx: i, rem: part % sum}
		assigned += base
	}

	sort.SliceStable(fracs, func(a, b int) bool {
		if fracs[a].rem != fracs[b].rem {
			return fracs[a].rem > fracs[b].rem
		}
		return fracs[a].idx < fracs[b].idx
	})
	for i := int64(0); i < amt-assigned; i++ {
		out[fracs[i%int64(n)].idx]++
	}

	if neg {
		for i := range out {
			out[i] = -out[i]
		}
	}
	return out
}

// Sum 汇总。
func Sum(items ...Money) Money {
	var s Money
	for _, m := range items {
		s += m
	}
	return s
}
