// Package calc 是纯函数计算内核：不碰数据库、不读时钟、无副作用，
// 输入 Facts + RuleSet 必然得到同一个 Result。
package calc

import (
	"fmt"
	"sort"
	"strings"

	"github.com/walry/yongjin/internal/domain"
	"github.com/walry/yongjin/internal/money"
	"github.com/walry/yongjin/internal/rules"
)

// 摘要与幂等键见 digest.go：Digest 会先规范化切片顺序。
// CatalogOf 从事实快照推导发布期体检所需的组合全集。
func CatalogOf(f domain.Facts) rules.Catalog {
	lines := map[domain.ProductLineID]struct{}{}
	for _, p := range f.Products {
		lines[p.Line] = struct{}{}
	}
	roles := map[domain.RoleID]struct{}{domain.RoleOwner: {}}
	for _, o := range f.Orders {
		for _, pp := range o.Participants {
			roles[pp.Role] = struct{}{}
		}
	}

	// 季度取自实际成单时间：只对事实里出现过的季度检查时间因子缺口，
	// 否则"明年 Q1"这种还没到的季度会天天告警。
	quarters := map[domain.Quarter]struct{}{}
	for _, o := range f.Orders {
		if q := domain.QuarterOf(o.ClosedAt); q != "" {
			quarters[q] = struct{}{}
		}
	}

	out := rules.Catalog{}
	for l := range lines {
		out.Lines = append(out.Lines, l)
	}
	for r := range roles {
		out.Roles = append(out.Roles, r)
	}
	for q := range quarters {
		out.Quarters = append(out.Quarters, q)
	}
	sort.Slice(out.Lines, func(i, j int) bool { return out.Lines[i] < out.Lines[j] })
	sort.Slice(out.Roles, func(i, j int) bool { return out.Roles[i] < out.Roles[j] })
	sort.Slice(out.Quarters, func(i, j int) bool { return out.Quarters[i] < out.Quarters[j] })
	return out
}

// Run 执行流水线（八段）：归集 → 基数 → 因子 → 直接佣金 → 分账 → 池领取 → 池分发 → 管理奖金 → 结算。
//
// 归集（阶段 0）必须先整期跑完：单笔佣金依赖达成率倍数，达成率依赖"这个人本期
// 一共成了多少单"。这是全流程唯一的前后依赖，所以流水线天然分两趟，不能边算边累积。
func Run(facts domain.Facts, rs domain.RuleSet, opts Options) (Result, error) {
	if opts.EngineVersion == "" {
		opts.EngineVersion = EngineVersion
	}

	digest, err := Digest(facts)
	if err != nil {
		return Result{}, err
	}

	cat := CatalogOf(facts)
	ix, err := rules.Compile(rs, cat)
	if err != nil {
		return Result{}, err
	}

	res := Result{
		CalcRunID:      runID(facts.Period, digest, rs.Version, opts.EngineVersion),
		Period:         facts.Period,
		EngineVersion:  opts.EngineVersion,
		RuleSetVersion: rs.Version,
		FactsDigest:    digest,
		Warnings:       append([]string{}, ix.Gaps...),
	}

	// 第 1 趟：阶段 0 —— 归集全期业绩，定每人每口径的达成率。
	atts, attWarn := attainmentsOf(facts, ix)
	res.Attainments = atts
	res.Warnings = append(res.Warnings, attWarn...)
	an := make(map[attKey]Attainment, len(atts))
	for _, a := range atts {
		an[attKey{Person: a.Person, KPI: a.KPI}] = a
	}

	gapWarned := map[string]bool{}

	// 时间因子按成单季度取。整期都缺 ClosedAt 时季度恒为空，按季度配的因子
	// 一条都不命中，表现为"整期少乘一个系数"——金额偏小却看不出异常，所以告警。
	if ix.HasKind(domain.PhaseFactor, domain.ActTimeFactor) {
		var missing []string
		for _, o := range facts.Orders {
			if o.ClosedAt.IsZero() {
				missing = append(missing, string(o.ID))
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"[facts] 订单 %s 缺少 ClosedAt，时间因子无法判定季度（按 1.0 处理）",
				strings.Join(missing, ",")))
		}
	}

	// 第 2 趟：阶段 1~3 —— 逐单算钱。
	for _, o := range facts.Orders {
		for _, ln := range o.Lines {
			prod, ok := facts.Products[ln.Product]
			if !ok {
				res.Warnings = append(res.Warnings, fmt.Sprintf("订单 %s 行 %s 的产品 %s 缺少快照", o.ID, ln.ID, ln.Product))
				continue
			}
			ctx := domain.MatchCtx{
				Line: prod.Line, Series: prod.Series,
				Territory: o.Territory, Quarter: domain.QuarterOf(o.ClosedAt),
				Tier: o.Tier, OrderClass: o.Class,
			}

			// 阶段 3：直接佣金。产品线 × 角色定基准费率，达成率档位给倍数，两个口径各算一笔。
			for _, pp := range o.Persons() {
				for _, kpi := range dualKPI {
					base := ln.BaseOf(kpi)
					if base <= 0 {
						continue // 该口径无可计佣基数（收入口径尤其常见），不产出明细
					}
					dctx := ctx
					dctx.Role = pp.Role
					dctx.KPI = kpi

					r, ok := ix.Best(domain.PhaseDirect, dctx, facts.Period)
					if !ok {
						// 两个口径往往同时缺规则，按 (订单行, 角色) 去重，避免一条真缺口刷两条告警。
						key := fmt.Sprintf("%s/%s/%s", o.ID, ln.ID, pp.Role)
						if !gapWarned[key] {
							gapWarned[key] = true
							res.Warnings = append(res.Warnings, fmt.Sprintf(
								"[gap] 订单 %s 行 %s 角色 %s 无直接佣金规则，按 0 处理", o.ID, ln.ID, pp.Role))
						}
						continue
					}

					bh, _ := pickBand(ix, facts, an, pp.Person, pp.Role, kpi)
					dctx.Band = bh.Band.ID

					// 因子阶段：时间/产品/区域各至多一条，与基准费率、档位倍数
					// 一起进同一条 ApplyChain，全链只舍入一次（设计 §6.1）。
					fhits := factorsOf(ix, dctx, facts.Period)
					chain := make([]int64, 0, 2+len(fhits))
					chain = append(chain, r.Action.BaseBPS, bh.CoeffBPS)
					chain = append(chain, factorBPS(fhits)...)

					amt := money.ApplyChain(base, chain...)
					if pp.Weight != 100 {
						amt = amt * money.Money(pp.Weight) / 100
					}
					afterWeight := amt
					applied := []domain.RuleID{r.ID}

					for _, ov := range ix.Overlays(domain.PhaseDirect, dctx, facts.Period) {
						switch ov.Target {
						case domain.OverlayAddRate:
							amt += money.ApplyRate(base, ov.BPS)
						case domain.OverlayCap:
							if ov.Cap != nil && amt > *ov.Cap {
								amt = *ov.Cap
							}
						case domain.OverlayExempt:
							amt = 0
						}
						applied = append(applied, ov.ID)
					}
					if amt < 0 {
						amt = 0
					}

					res.Directs = append(res.Directs, DirectLine{
						OrderID: o.ID, LineID: ln.ID, Person: pp.Person, Role: pp.Role,
						Product: prod.ID, KPI: kpi, Base: base, RateBPS: r.Action.BaseBPS,
						BandID: bh.Band.ID, CoeffBPS: bh.CoeffBPS, Factors: fhits,
						Weight: pp.Weight, OverlayDelta: amt - afterWeight,
						Amount: amt, ShareMode: r.Action.ShareMode,
						RuleID: r.ID, Overlays: applied[1:],
					})

					res.Trace = append(res.Trace, Trace{
						OrderID: string(o.ID), LineID: string(ln.ID), Phase: string(domain.PhaseDirect),
						RuleID: string(r.ID), Person: string(pp.Person),
						KPI: string(kpi), Band: string(bh.Band.ID),
						Base: base, RateBPS: r.Action.BaseBPS, CoeffBPS: bh.CoeffBPS,
						Factors: fhits, Weight: pp.Weight, OverlayDelta: amt - afterWeight,
						Amount: amt, Note: overlayNote(applied),
					})
				}
			}
		}
	}

	// 输出确定性：明细按业务标识排序，使结果与订单/行的录入顺序无关。
	// 排序键不用下标，也不依赖 facts 切片顺序，否则同一份事实换个录入次序就得到不同输出。
	sort.Slice(res.Directs, func(i, j int) bool {
		a, b := res.Directs[i], res.Directs[j]
		if a.OrderID != b.OrderID {
			return a.OrderID < b.OrderID
		}
		if a.LineID != b.LineID {
			return a.LineID < b.LineID
		}
		if a.Person != b.Person {
			return a.Person < b.Person
		}
		if a.Role != b.Role {
			return a.Role < b.Role
		}
		return a.KPI < b.KPI
	})

	// I11：同一 (订单行, 人, 口径) 至多一条明细，两个口径不得合并成一条。
	for i := 1; i < len(res.Directs); i++ {
		a, b := res.Directs[i-1], res.Directs[i]
		if a.OrderID == b.OrderID && a.LineID == b.LineID && a.Person == b.Person && a.KPI == b.KPI {
			return Result{}, fmt.Errorf("I11 违反：%s/%s/%s/%s 出现多条直接佣金明细",
				a.OrderID, a.LineID, a.Person, a.KPI)
		}
	}

	// 阶段 4：分账（设计 §7）。一次零和转移，不级联；只改钱落在谁账上，
	// 绝不回写 DirectLine.Amount（管理奖金基数读的是分账前全额，I12）。
	splits, shareWarn := splitsOf(facts, res.Directs)
	if err := assertSplits(res.Directs, splits); err != nil {
		return Result{}, err
	}
	res.Splits = splits
	res.Warnings = append(res.Warnings, shareWarn...)

	// 阶段 5~6：池领取 + 池分发。预算来自事实，领取按成单全序 FIFO，
	// 产物分层保留（PoolClaim / Distribution），见 pool.go。
	pr, err := runPools(facts, ix)
	if err != nil {
		return Result{}, err
	}
	res.Claims = pr.claims
	res.Distributions = pr.dists
	res.Pools = pr.pools
	res.Trace = append(res.Trace, pr.traces...)
	res.Warnings = append(res.Warnings, pr.warnings...)

	// 阶段 7：管理奖金。走组织树上溯，逐层衰减。池分成不参与（§8.4）。
	// 基数是分账前的 DirectLine.Amount 全额，合约/收入两条分别上溯（设计 §7.1 方案 A）。
	mrules := map[domain.KPI]domain.Rule{}
	for _, kpi := range dualKPI {
		if r, ok := ix.Best(domain.PhaseManager, domain.MatchCtx{KPI: kpi}, facts.Period); ok {
			mrules[kpi] = r
		} else {
			res.Warnings = append(res.Warnings, fmt.Sprintf("[gap] 口径 %s 无管理奖金规则", kpi))
		}
	}
	for _, d := range res.Directs {
		mrule, ok := mrules[d.KPI]
		if !ok {
			continue
		}
		m := facts.ManagerOf(d.Person)
		for level := 1; m != "" && level <= mrule.Action.ChainDepth && level-1 < len(mrule.Action.DecayBPS); level++ {
			bps := mrule.Action.DecayBPS[level-1]
			amt := money.ApplyRate(d.Amount, bps)
			if amt > 0 {
				res.ManagerBonuses = append(res.ManagerBonuses, ManagerBonus{
					OrderID: d.OrderID, LineID: d.LineID,
					Source: d.Person, Manager: m, KPI: d.KPI, Level: level,
					Base: d.Amount, RateBPS: bps, Amount: amt, RuleID: mrule.ID,
				})
				res.Trace = append(res.Trace, Trace{
					OrderID: string(d.OrderID), LineID: string(d.LineID),
					Phase: string(domain.PhaseManager), RuleID: string(mrule.ID),
					Person: string(m), KPI: string(d.KPI),
					Base: d.Amount, RateBPS: bps, CoeffBPS: money.BPS, Amount: amt,
					Note: fmt.Sprintf("manager of %s level=%d", d.Person, level),
				})
			}
			m = facts.ManagerOf(m)
		}
	}

	// 阶段 8：按人结算。聚合规则只有一份实现（aggregate.go），投影复用同一个函数。
	res.Persons = aggregatePersonTotals(res.Directs, res.Splits, res.Distributions, res.ManagerBonuses, facts.PersonName)

	// 守恒断言：池分完 + 结转；人员合计 = 直接 + 池 + 管理。
	for _, p := range res.Pools {
		if p.Distributed+p.CarryOver != p.Budget {
			return Result{}, fmt.Errorf("I2 违反：池 %s 失衡", p.Pool)
		}
	}
	var check money.Money
	for _, p := range res.Persons {
		check += p.Total
	}
	// I6：分账净额全公司求和必须为 0，否则说明有人凭空多拿或少拿。
	var splitNet money.Money
	for _, p := range res.Persons {
		splitNet += p.Split
	}
	if splitNet != 0 {
		return Result{}, fmt.Errorf("I6 违反：分账净额合计 %s 不为 0", splitNet)
	}

	var expect money.Money
	for _, d := range res.Directs {
		expect += d.Amount
	}
	for _, d := range res.Distributions {
		expect += d.Amount
	}
	for _, m := range res.ManagerBonuses {
		expect += m.Amount
	}
	if check != expect {
		return Result{}, fmt.Errorf("人员合计 %s 与明细合计 %s 不一致", check, expect)
	}

	return res, nil
}

func overlayNote(ids []domain.RuleID) string {
	if len(ids) <= 1 {
		return ""
	}
	parts := make([]string, 0, len(ids)-1)
	for _, id := range ids[1:] {
		parts = append(parts, "overlay="+string(id))
	}
	return join(parts, ",")
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
