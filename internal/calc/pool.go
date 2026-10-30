package calc

import (
	"fmt"
	"sort"

	"github.com/walry/yongjin/internal/domain"
	"github.com/walry/yongjin/internal/money"
	"github.com/walry/yongjin/internal/rules"
)

// 奖金池分两层（设计 §8.1 / §8.2）：
//
//	阶段 5 领取：预算有限，按 (ClosedAt, OrderID, LineID) 全序把预算消耗掉 → PoolClaim
//	阶段 6 分发：每笔领取额在这一单的参与人之间怎么分 → Distribution
//
// 两层不能合并。合并后只剩「谁拿了多少钱」，于是「池没发完是因为订单不够，还是
// 规则没配」变成同一个现象；而前者是正常结转，后者是配置缺口，必须能分别看见。
//
// 领取顺序就是 FIFO，不是可选项：预算有限时，谁先成单谁先领满。把它做成 DistMode
// 的一个枚举会把「何时领」和「谁来分」搅在一起，反而两件事都说不清。

// poolLine 是全序里的一个订单行，连同它所属的订单（分发时要看参与者）。
type poolLine struct {
	order domain.Order
	line  domain.OrderLine
}

// poolLines 按 (ClosedAt, OrderID, LineID) 升序返回全部订单行。
// 不排序就等于让事实切片的录入次序决定谁拿到钱（I9）。
func poolLines(facts domain.Facts) []poolLine {
	var out []poolLine
	for _, o := range facts.Orders {
		for _, ln := range o.Lines {
			out = append(out, poolLine{order: o, line: ln})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.order.ClosedAt.Equal(b.order.ClosedAt) {
			return a.order.ClosedAt.Before(b.order.ClosedAt)
		}
		if a.order.ID != b.order.ID {
			return a.order.ID < b.order.ID
		}
		return a.line.ID < b.line.ID
	})
	return out
}

// poolState 是一个池在计算期内的运行状态。预算来自事实（PoolDef.Budget），
// 规则只决定「每单领多少」，再也不负责「池子有多大」—— 这是 M5 的核心变化：
// 池是预算，不是费率（设计 §2.3）。
type poolState struct {
	budget   money.Money
	hasDef   bool
	claim    domain.RuleID
	dist     domain.Rule
	hasDist  bool
	claimed  money.Money
	retained money.Money
	paid     money.Money
}

// poolResult 是池阶段的三份产物：领取明细、分发明细、期末对账，外加留痕与告警。
type poolResult struct {
	claims   []PoolClaim
	dists    []Distribution
	pools    []PoolSettlement
	traces   []Trace
	warnings []string
}

// runPools 执行阶段 5 与 6。一次遍历同时算两层，但产物分开保存。
func runPools(facts domain.Facts, ix *rules.Index) (poolResult, error) {
	var out poolResult
	states := map[domain.PoolID]*poolState{}
	get := func(pid domain.PoolID) *poolState {
		s, ok := states[pid]
		if !ok {
			s = &poolState{}
			states[pid] = s
		}
		return s
	}

	// 预算来自事实。期间不匹配的定义忽略：池是按期次的预算，不是长期账户。
	for _, d := range facts.Pools {
		if d.Period != "" && d.Period != facts.Period {
			continue
		}
		s := get(d.ID)
		s.budget += d.Budget
		s.hasDef = true
	}

	short := map[domain.PoolID]int{} // 没领满的笔数（耗尽或不足），收尾统一告警

	for _, pl := range poolLines(facts) {
		prod, ok := facts.Products[pl.line.Product]
		if !ok {
			continue // 直接佣金阶段会就这条产品缺失告警，这里不重复刷屏
		}
		// 领取口径是收入口径（设计 §5.2）：池奖励的是实际业绩，不是签约金额。
		// 若改成合同口径，同一产品线的池会被两条口径各领一次，总量翻倍。
		base := pl.line.BaseOf(domain.KPIRevenue)
		if base <= 0 {
			continue
		}
		ctx := domain.MatchCtx{
			Line: prod.Line, Series: prod.Series,
			Territory: pl.order.Territory, Quarter: domain.QuarterOf(pl.order.ClosedAt),
			Tier: pl.order.Tier, OrderClass: pl.order.Class, KPI: domain.KPIRevenue,
		}

		// 领取是**按行**的：一行对同一个池最多领一次，否则总量会随参与人数翻倍。
		// 但规则可以按角色分池（§8.3 的负责人池），所以按参与角色各查一次、再按池去重。
		// 角色先排序：让"同一个池被多个角色规则命中"这种配置也有确定的胜者（I9）。
		taken := map[domain.PoolID]bool{}
		for _, role := range rolesOf(pl.order) {
			lctx := ctx
			lctx.Role = role
			// 一行可以同时喂多个池（产品线池 + 负责人池），所以是 All 不是 Best；
			// 指向同一个池的多条规则才互相裁决（rules.arbitrationKey 按 Target 分域）。
			for _, r := range ix.All(domain.PhaseFund, lctx, facts.Period) {
				pid := domain.PoolID(r.Action.Target)
				if pid == "" || taken[pid] {
					continue
				}
				rate := claimBPS(r.Action, base)
				want := money.ApplyRate(base, rate)
				if want <= 0 {
					continue
				}
				taken[pid] = true

				s := get(pid)
				if s.claim == "" {
					s.claim = r.ID
				}
				remainBefore := s.budget - s.claimed
				claimed := want
				if claimed > remainBefore {
					claimed = remainBefore
				}
				if claimed < 0 {
					claimed = 0
				}
				s.claimed += claimed

				out.claims = append(out.claims, PoolClaim{
					OrderID: pl.order.ID, LineID: pl.line.ID, Pool: pid,
					Budget: s.budget, Remaining: remainBefore,
					Claimed: claimed, Exhausted: s.claimed >= s.budget, RuleID: r.ID,
				})
				out.traces = append(out.traces, Trace{
					OrderID: string(pl.order.ID), LineID: string(pl.line.ID), Phase: string(domain.PhaseFund),
					RuleID: string(r.ID), Pool: string(pid), KPI: string(domain.KPIRevenue),
					Base: base, RateBPS: rate, CoeffBPS: money.BPS,
					// 预算封顶是「由外部约束截断」，不是倍率链的一环，所以走 OverlayDelta：
					// 这样 Trace.Replay() 仍能逐分复算出 Claimed，留痕不会因封顶而失真（I8）。
					OverlayDelta: claimed - want,
					Amount:       claimed,
					Note:         fmt.Sprintf("pool=%s remain_before=%s", pid, remainBefore),
				})

				if claimed < want {
					short[pid]++
				}
				if claimed == 0 {
					continue // 池已空：分发阶段没有钱可分，但仍要留上面这条领取零额的记录
				}
				distributeClaim(&out, facts, ix, pl, s, pid, claimed)
			}
		}
	}

	// 期末对账：每个池都要出现，包括「有预算但一笔没领」和「被规则引用但没预算定义」。
	ids := make([]string, 0, len(states))
	for pid := range states {
		ids = append(ids, string(pid))
	}
	sort.Strings(ids)
	for _, sid := range ids {
		pid := domain.PoolID(sid)
		s := states[pid]
		if !s.hasDef {
			out.warnings = appendOnce(out.warnings, fmt.Sprintf(
				"[pool] 池 %s 被规则引用但事实里没有预算定义（Budget=0，全部按 0 领取）", pid))
		}
		// I2：CarryOver 是「预算里没发出去的全部」，因此它天然包含 Retained，
		// 两者不是并列的两份钱。写成 Budget − Distributed 让式子恒成立。
		carry := s.budget - s.paid
		out.pools = append(out.pools, PoolSettlement{
			Pool: pid, Budget: s.budget, Claimed: s.claimed, Retained: s.retained,
			Distributed: s.paid, CarryOver: carry,
			ClaimRule: s.claim, DistRule: s.dist.ID,
		})
		if s.paid+carry != s.budget {
			return poolResult{}, fmt.Errorf("I2 违反：池 %s budget=%s distributed=%s carry=%s",
				pid, s.budget, s.paid, carry)
		}
	}

	pids := make([]string, 0, len(short))
	for pid := range short {
		pids = append(pids, string(pid))
	}
	sort.Strings(pids)
	for _, pid := range pids {
		out.warnings = append(out.warnings, fmt.Sprintf(
			"[pool] 池 %s 预算不足，%d 笔未领满（差额结转）", pid, short[domain.PoolID(pid)]))
	}

	return out, nil
}

// rolesOf 返回订单涉及的角色，去重后按名字升序。
// 排序不是为了好看：同一个池被多个角色规则命中时，胜者由遍历次序决定，
// 不排序就等于让参与者切片的录入顺序决定金额（I9）。
func rolesOf(o domain.Order) []domain.RoleID {
	seen := map[domain.RoleID]bool{}
	var out []domain.RoleID
	for _, pp := range o.Persons() {
		if seen[pp.Role] {
			continue
		}
		seen[pp.Role] = true
		out = append(out, pp.Role)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// distributeClaim 执行阶段 6：把一笔领取额扣除留存后分给参与人。
// 分不出去的钱退回留存（→ 结转）并告警，绝不凭空消失。
func distributeClaim(out *poolResult, facts domain.Facts, ix *rules.Index, pl poolLine, s *poolState, pid domain.PoolID, claimed money.Money) {
	if !s.hasDist {
		if r, ok := ix.Best(domain.PhaseDistribute, domain.MatchCtx{Pool: pid}, facts.Period); ok {
			s.dist, s.hasDist = r, true
		}
	}
	if !s.hasDist {
		// 缺口已在 Compile 报过（rules.detectGaps），这里只负责把钱留住，不重复告警。
		s.retained += claimed
		return
	}

	retained := money.ApplyRate(claimed, s.dist.Action.RetainBPS)
	distributable := claimed - retained
	s.retained += retained

	dists, reason := distribute(s.dist, pl, distributable)
	if reason != "" {
		out.warnings = appendOnce(out.warnings, fmt.Sprintf(
			"[pool] 池 %s %s，%s 留在池中结转", pid, reason, distributable))
		s.retained += distributable
		return
	}
	for _, d := range dists {
		s.paid += d.Amount
		out.dists = append(out.dists, d)
	}
}

// claimBPS 返回这一行适用的领取费率。MethodTier 允许按基数分档，
// 但金额仍然只由「基数 × 费率」得到 —— 与直接佣金一样，别在池上另开一套算法。
func claimBPS(a domain.Action, base money.Money) int64 {
	if a.Method == domain.MethodTier {
		for _, t := range a.Tiers {
			if t.UpTo == 0 || base <= t.UpTo {
				return t.BPS
			}
		}
		return 0
	}
	return a.BaseBPS
}

// distribute 把一笔领取额分给这一单的参与人（设计 §8.2）。返回的分录已按人排好序，
// reason 非空表示分不出去（角色不在场、权重全零），调用方负责退回结转。
func distribute(r domain.Rule, pl poolLine, amount money.Money) ([]Distribution, string) {
	if amount <= 0 {
		return nil, ""
	}
	a := r.Action
	people := pl.order.Persons()

	type slot struct {
		p domain.PersonID
		r domain.RoleID
		w int64
	}
	var slots []slot
	switch a.DistMode {
	case domain.DistEqual:
		// 均分：负责人 + 全部协作人，各一份。份额用权重 1 交给 Allocate，
		// 不先算 1/N 的整数百分比再乘——那会把尾差丢在舍入里。
		for _, pp := range people {
			slots = append(slots, slot{pp.Person, pp.Role, 1})
		}
	case domain.DistAll:
		// 「所有」= 全给指定角色。没指定角色说明规则没配全，按分不出去处理，
		// 不要默认给负责人 —— 默认值会把配置错误变成一笔无声的、金额正确的错发。
		if a.AllocTo == "" {
			return nil, "DistMode=all 但未指定 AllocTo"
		}
		for _, pp := range people {
			if pp.Role == a.AllocTo {
				slots = append(slots, slot{pp.Person, pp.Role, 1})
			}
		}
	case domain.DistWeight:
		for _, pp := range people {
			rate, ok := a.WeightTable[pp.Role]
			if !ok || rate <= 0 {
				continue // 表里没有的角色不参与，不做「按 1 兜底」——那是静默错发
			}
			// 参与人自身的 Weight 是这次协作的份额（50 = 半分），与角色权重相乘。
			w := pp.Weight * rate / 100
			if w <= 0 {
				continue
			}
			slots = append(slots, slot{pp.Person, pp.Role, w})
		}
	default:
		return nil, "分发方式未配置"
	}

	if len(slots) == 0 {
		return nil, "无参与人命中分发方式"
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].p < slots[j].p })

	weights := make([]int64, len(slots))
	for i, s := range slots {
		weights[i] = s.w
	}
	parts := money.Allocate(amount, weights)

	out := make([]Distribution, 0, len(slots))
	for i, s := range slots {
		if parts[i] == 0 {
			continue
		}
		out = append(out, Distribution{
			OrderID: pl.order.ID, LineID: pl.line.ID, Pool: domain.PoolID(a.Target),
			Person: s.p, Role: s.r, Weight: s.w, Amount: parts[i], RuleID: r.ID,
		})
	}
	return out, ""
}

// appendOnce 加一条告警，已存在就跳过 —— 同一条规则会被几十单命中，
// 逐单重复刷屏会把真正的缺口埋掉。
func appendOnce(list []string, msg string) []string {
	for _, s := range list {
		if s == msg {
			return list
		}
	}
	return append(list, msg)
}
