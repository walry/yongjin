package calc

import (
	"github.com/walry/yongjin/internal/domain"
	"github.com/walry/yongjin/internal/rules"
)

// factorKinds 是因子链的固定顺序：时间 → 产品 → 区域。
// 乘法可交换，所以顺序不影响金额；固定顺序是为了 Trace 稳定、便于逐字比对。
var factorKinds = []domain.ActionKind{
	domain.ActTimeFactor, domain.ActProductFactor, domain.ActTerritoryFactor,
}

// FactorHit 是一次因子命中：哪个 Kind、哪条规则、倍数多少。
// 它同时进 DirectLine 与 Trace —— 两者都不能只留金额，否则改档后无法回答
// "这笔钱当年是按什么乘出来的"。
type FactorHit struct {
	Kind   domain.ActionKind
	RuleID domain.RuleID
	BPS    int64 // 倍数，10000 = 1.0
}

// factorsOf 收集本次计算命中的因子：每个 Kind 至多一条，不同 Kind 相乘。
//
// 未命中的 Kind 不产出记录，等价于倍数 1.0 —— 而不是 0。漏配因子是
// "少乘"，配成 0 是"清零"，两者绝不能混：后者会把整条佣金链抹平。
func factorsOf(ix *rules.Index, ctx domain.MatchCtx, period domain.Period) []FactorHit {
	out := make([]FactorHit, 0, len(factorKinds))
	for _, kind := range factorKinds {
		r, ok := ix.BestByKind(domain.PhaseFactor, kind, ctx, period)
		if !ok || r.Action.MultiplierBPS <= 0 {
			continue
		}
		out = append(out, FactorHit{Kind: kind, RuleID: r.ID, BPS: r.Action.MultiplierBPS})
	}
	return out
}

// factorBPS 把因子摊平成 bps 序列，供 money.ApplyChain 一次舍入。
func factorBPS(hits []FactorHit) []int64 {
	out := make([]int64, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.BPS)
	}
	return out
}
