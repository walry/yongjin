// Package rules 负责把规则集编译成可查询索引：特异性裁决、冲突检测、缺口报告。
package rules

import (
	"fmt"
	"sort"
	"strings"

	"github.com/walry/yongjin/internal/domain"
	"github.com/walry/yongjin/internal/money"
)

// Ctx 是查找上下文，直接复用 domain 的定义避免两套类型漂移。
type Ctx = domain.MatchCtx

// Catalog 是发布期体检用的全量组合，来源于事实快照中的产品线与角色。
type Catalog struct {
	Lines    []domain.ProductLineID
	Roles    []domain.RoleID
	Quarters []domain.Quarter // 事实里实际出现的成单季度，只对这些季度检查时间因子缺口
}

type Index struct {
	version  string
	byPhase  map[domain.Phase][]domain.Rule
	overlays []domain.Overlay

	// Gaps 与 Conflicts 是发布闸门的产物：有冲突则直接拒绝发布。
	Gaps      []string
	Conflicts []string
}

func (ix *Index) Version() string { return ix.version }

// Compile 计算特异性、排序、检测冲突与缺口。冲突不是警告而是错误。
func Compile(rs domain.RuleSet, cat Catalog) (*Index, error) {
	ix := &Index{version: rs.Version, byPhase: map[domain.Phase][]domain.Rule{}, overlays: rs.Overlays}

	for _, r := range rs.Rules {
		r.Scope.ComputeSpecificity()
		ix.byPhase[r.Phase] = append(ix.byPhase[r.Phase], r)
	}
	for p := range ix.byPhase {
		list := ix.byPhase[p]
		sort.SliceStable(list, func(a, b int) bool { return better(&list[a], &list[b]) })
		ix.byPhase[p] = list
	}

	ix.detectConflicts()
	ix.detectBandGaps()
	ix.detectAttainScope()
	ix.detectFactorRules()
	ix.detectTerritoryMisuse()
	ix.detectShareRules()
	ix.detectPoolRules()
	ix.detectGaps(cat)
	sort.Strings(ix.Conflicts)
	sort.Strings(ix.Gaps)

	if len(ix.Conflicts) > 0 {
		return ix, fmt.Errorf("规则集 %s 存在 %d 处无法裁决的冲突: %s",
			rs.Version, len(ix.Conflicts), strings.Join(ix.Conflicts, " | "))
	}
	return ix, nil
}

// better 是唯一的裁决顺序，运行时与编译期共用，保证一致。
// 特异性 > 优先级 > 更晚生效 > ID 字典序（仅用于稳定输出）。
func better(a, b *domain.Rule) bool {
	if a.Scope.Specificity != b.Scope.Specificity {
		return a.Scope.Specificity > b.Scope.Specificity
	}
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	if a.EffectiveFrom != b.EffectiveFrom {
		return a.EffectiveFrom > b.EffectiveFrom
	}
	return a.ID < b.ID
}

func (ix *Index) Best(phase domain.Phase, ctx Ctx, period domain.Period) (domain.Rule, bool) {
	return ix.bestIn(phase, "", ctx, period)
}

// BestByKind 只在一个 ActionKind 内裁决。因子阶段的规则按 Kind 各取一条再相乘，
// 若用 Best 会退化成"三条因子只生效一条"。
func (ix *Index) BestByKind(phase domain.Phase, kind domain.ActionKind, ctx Ctx, period domain.Period) (domain.Rule, bool) {
	return ix.bestIn(phase, kind, ctx, period)
}

// HasKind 判断阶段内是否存在指定 Kind 的规则。用于"事实缺字段要不要告警"这类判断：
// 没有时间因子规则时，订单缺 ClosedAt 不算问题。
func (ix *Index) HasKind(phase domain.Phase, kind domain.ActionKind) bool {
	for _, r := range ix.byPhase[phase] {
		if kind == "" || r.Action.Kind == kind {
			return true
		}
	}
	return false
}

// All 返回某阶段内命中的**每个裁决域各一条**规则，按域键排序。
//
// 只给"同一行可以同时落进多个独立目标"的阶段用（池领取：一行可同时喂产品线池
// 与负责人池）。其余阶段必须用 Best —— 同一件事命中两条规则是冲突，不是叠加。
func (ix *Index) All(phase domain.Phase, ctx Ctx, period domain.Period) []domain.Rule {
	best := map[string]domain.Rule{}
	for _, r := range ix.byPhase[phase] {
		if !r.Effective(period) || !r.Scope.Matches(ctx) {
			continue
		}
		k := arbitrationKey(r)
		cur, ok := best[k]
		if !ok || better(&r, &cur) {
			best[k] = r
		}
	}
	keys := make([]string, 0, len(best))
	for k := range best {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]domain.Rule, 0, len(keys))
	for _, k := range keys {
		out = append(out, best[k])
	}
	return out
}
func (ix *Index) bestIn(phase domain.Phase, kind domain.ActionKind, ctx Ctx, period domain.Period) (domain.Rule, bool) {
	var best domain.Rule
	found := false
	for _, r := range ix.byPhase[phase] {
		if kind != "" && r.Action.Kind != kind {
			continue
		}
		if !r.Effective(period) || !r.Scope.Matches(ctx) {
			continue
		}
		if !found || better(&r, &best) {
			best = r
			found = true
		}
	}
	return best, found
}

// Overlays 返回当前上下文命中的特殊规则，按优先级降序。
func (ix *Index) Overlays(phase domain.Phase, ctx Ctx, period domain.Period) []domain.Overlay {
	var out []domain.Overlay
	for _, o := range ix.overlays {
		if o.Phase != phase || !o.Effective(period) || !o.Scope.Matches(ctx) {
			continue
		}
		out = append(out, o)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Priority != out[b].Priority {
			return out[a].Priority > out[b].Priority
		}
		return out[a].ID < out[b].ID
	})
	return out
}

func rangesOverlap(a, b domain.Rule) bool {
	if a.EffectiveTo != "" && b.EffectiveFrom != "" && a.EffectiveTo < b.EffectiveFrom {
		return false
	}
	if b.EffectiveTo != "" && a.EffectiveFrom != "" && b.EffectiveTo < a.EffectiveFrom {
		return false
	}
	return true
}

// arbitrationKey 是"裁决域"：只有同一域内的两条规则才互相竞争、才可能冲突。
//
// 因子阶段按 Kind 各取一条再相乘（时间 × 产品 × 区域），所以三个 Kind 各自成域：
// 一条区域因子与一条时间因子同时命中不是冲突，是相乘。
//
// 池领取按**目标池**分域：同一产品线可并存产品线池与负责人池（设计 §8.3），
// 两条规则命中同一行是各领各的，不是冲突；只有指向同一个池的两条重叠规则才互斥。
// 其余阶段整段一个域。
func arbitrationKey(r domain.Rule) string {
	switch r.Phase {
	case domain.PhaseFactor:
		return string(r.Phase) + "/" + string(r.Action.Kind)
	case domain.PhaseFund:
		return string(r.Phase) + "/" + r.Action.Target
	}
	return string(r.Phase)
}

func (ix *Index) detectConflicts() {
	groups := map[string][]domain.Rule{}
	for _, list := range ix.byPhase {
		for _, r := range list {
			k := arbitrationKey(r)
			groups[k] = append(groups[k], r)
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		list := groups[k]
		for i := 0; i < len(list); i++ {
			for j := i + 1; j < len(list); j++ {
				a, b := list[i], list[j]
				if a.Scope.Specificity != b.Scope.Specificity || a.Priority != b.Priority {
					continue
				}
				if !rangesOverlap(a, b) || !a.Scope.Overlaps(b.Scope) {
					continue
				}
				ix.Conflicts = append(ix.Conflicts, fmt.Sprintf(
					"[%s] %s vs %s (specificity=%d priority=%d)",
					k, a.ID, b.ID, a.Scope.Specificity, a.Priority))
			}
		}
	}
}

// detectBandGaps 校验档位表本身。档位是乘法因子，取不到档 = 少发钱且不报错，
// 所以这里一律 error：倍数非单调/有洞/有重叠/缺上下界，都是配置错误而非风格问题。
func (ix *Index) detectBandGaps() {
	for phase, list := range ix.byPhase {
		for _, r := range list {
			if phase != domain.PhaseAttain {
				if len(r.Action.Bands) > 0 {
					ix.Conflicts = append(ix.Conflicts,
						fmt.Sprintf("[%s] %s 配了档位表：档位只能出现在 attain 阶段", phase, r.ID))
				}
				continue
			}
			ix.checkBandTable(r)
		}
	}
}

func (ix *Index) checkBandTable(r domain.Rule) {
	bands := append([]domain.Band(nil), r.Action.Bands...)
	if len(bands) == 0 {
		ix.Conflicts = append(ix.Conflicts, fmt.Sprintf("[attain] %s 未配置档位表", r.ID))
		return
	}
	sort.SliceStable(bands, func(i, j int) bool { return bands[i].FromBPS < bands[j].FromBPS })
	if bands[0].FromBPS != 0 {
		ix.Conflicts = append(ix.Conflicts,
			fmt.Sprintf("[attain] %s 最低档 FromBPS=%d：必须从 0 起，否则低达成率的人无档可命", r.ID, bands[0].FromBPS))
	}
	if last := bands[len(bands)-1]; last.ToBPS != 0 {
		ix.Conflicts = append(ix.Conflicts,
			fmt.Sprintf("[attain] %s 最高档 ToBPS=%d：必须为 0（无上界）", r.ID, last.ToBPS))
	}
	for i, b := range bands {
		if b.CoeffBPS <= 0 {
			ix.Conflicts = append(ix.Conflicts,
				fmt.Sprintf("[attain] %s 档位 %s 倍数 %d 必须为正", r.ID, b.ID, b.CoeffBPS))
		}
		if i == 0 {
			continue
		}
		prev := bands[i-1]
		if prev.ToBPS != b.FromBPS {
			ix.Conflicts = append(ix.Conflicts, fmt.Sprintf(
				"[attain] %s 档位 %s..%s 不连续：上档止于 %d，本档起于 %d",
				r.ID, prev.ID, b.ID, prev.ToBPS, b.FromBPS))
		}
		if b.CoeffBPS < prev.CoeffBPS {
			ix.Conflicts = append(ix.Conflicts, fmt.Sprintf(
				"[attain] %s 倍数非单调：%s=%d 高于 %s=%d",
				r.ID, prev.ID, prev.CoeffBPS, b.ID, b.CoeffBPS))
		}
	}
}

// detectAttainScope 限制 attain 规则的作用域：达成率是"人员 × 期间"指标，
// 与具体订单无关，所以按产品线/区域/档位配作用域没有语义，配了也永远不会命中。
func (ix *Index) detectAttainScope() {
	for _, r := range ix.byPhase[domain.PhaseAttain] {
		s := r.Scope
		bad := len(s.Lines) + len(s.Series) + len(s.Pools) + len(s.Territories) +
			len(s.Tiers) + len(s.Bands) + len(s.OrderClasses) + len(s.Quarters)
		if bad > 0 {
			ix.Conflicts = append(ix.Conflicts, fmt.Sprintf(
				"[attain] %s 作用域只允许 角色/部门/口径：达成率不按订单维度或季度切分", r.ID))
		}
	}
}

// factorKinds 是允许出现在因子阶段的三种动作，与 calc.factorKinds 一一对应。
var factorKinds = []domain.ActionKind{
	domain.ActTimeFactor, domain.ActProductFactor, domain.ActTerritoryFactor,
}

func isFactorKind(k domain.ActionKind) bool {
	for _, x := range factorKinds {
		if x == k {
			return true
		}
	}
	return false
}

// detectFactorRules 校验因子规则本身。这一层的错误全部是"配了但永远不生效"或
// "配了但把钱乘成 0"，运行时看不出来，只能在发布期拦住。
func (ix *Index) detectFactorRules() {
	for phase, list := range ix.byPhase {
		for _, r := range list {
			if phase != domain.PhaseFactor {
				if isFactorKind(r.Action.Kind) {
					ix.Conflicts = append(ix.Conflicts, fmt.Sprintf(
						"[%s] %s 是因子动作：因子规则只能出现在 factor 阶段", phase, r.ID))
				}
				continue
			}
			if !isFactorKind(r.Action.Kind) {
				ix.Conflicts = append(ix.Conflicts, fmt.Sprintf(
					"[factor] %s 动作 %q 不是因子：只允许 time/product/territory", r.ID, r.Action.Kind))
				continue
			}
			// 0 会把整条链乘成 0，而"想清零"应该用 Overlay exempt。
			if r.Action.MultiplierBPS <= 0 {
				ix.Conflicts = append(ix.Conflicts, fmt.Sprintf(
					"[factor] %s 倍数 %d 必须为正（10000 = 1.0）", r.ID, r.Action.MultiplierBPS))
			}
			// 产品因子按系列配时必须同时写明产品线：series 特异性 4 < line 8，
			// 只写系列的规则永远被产品线规则压过 —— 静默失效，比报错危险得多。
			if r.Action.Kind == domain.ActProductFactor &&
				len(r.Scope.Series) > 0 && len(r.Scope.Lines) == 0 {
				ix.Conflicts = append(ix.Conflicts, fmt.Sprintf(
					"[factor] %s 按系列配了产品因子却没写产品线：特异性 4 会被产品线规则压过", r.ID))
			}
		}
	}
}

// detectTerritoryMisuse 禁止区域进入直接佣金与归集阶段。区域是乘数（PhaseFactor），
// 若同时当筛选维度，同一维度会被乘两次，而结果上 1.1×1.1 与"配成 1.21"无法区分。
func (ix *Index) detectTerritoryMisuse() {
	for _, phase := range []domain.Phase{domain.PhaseDirect, domain.PhaseAttain} {
		for _, r := range ix.byPhase[phase] {
			if len(r.Scope.Territories) > 0 {
				ix.Conflicts = append(ix.Conflicts, fmt.Sprintf(
					"[%s] %s 配了区域作用域：区域只能出现在 factor 阶段作为乘数", phase, r.ID))
			}
		}
	}
}

// allRules 按阶段名排序返回全部规则，保证冲突/缺口消息的顺序稳定。
func (ix *Index) allRules() []domain.Rule {
	phases := make([]string, 0, len(ix.byPhase))
	for p := range ix.byPhase {
		phases = append(phases, string(p))
	}
	sort.Strings(phases)
	out := make([]domain.Rule, 0, len(phases))
	for _, p := range phases {
		out = append(out, ix.byPhase[domain.Phase(p)]...)
	}
	return out
}

// detectShareRules 拦两种分账配置错误。两者都属于「运行时既不生效也不报错」：
//
//   - 分账方式配在非 PhaseDirect 阶段：分账只作用于直接佣金，配在池/管理奖上永远不会被读到。
//   - 分账方式写了未知取值：会一路传到运行时，shareReceivers 找不到分支，静默按全额自留。
func (ix *Index) detectShareRules() {
	for _, r := range ix.allRules() {
		if r.Action.ShareMode == domain.ShareNone {
			continue
		}
		switch r.Action.ShareMode {
		case domain.SharePair1to1, domain.ShareSplitSubs:
		default:
			ix.Conflicts = append(ix.Conflicts, fmt.Sprintf(
				"[share] %s 分账方式 %q 未定义（只允许 pair1to1 / splitSubordinates）",
				r.ID, r.Action.ShareMode))
			continue
		}
		if r.Phase != domain.PhaseDirect {
			ix.Conflicts = append(ix.Conflicts, fmt.Sprintf(
				"[share] %s 分账方式 %s 只能配在 PhaseDirect（当前 %s）",
				r.ID, r.Action.ShareMode, r.Phase))
		}
	}
}

// detectPoolRules 拦四种池分发配置错误。共同点是「配了但永远读不到，或者读到了也不对」：
//
//   - 分发方式配在非 PhaseDistribute 阶段：池两层是领取与分发，配在 fund 上没人读。
//   - 分发方式写了未知取值：runPools 会走 default 分支，钱全额退回结转 —— 看似安全，
//     实际是一笔领了又没发的静默错配。
//   - DistMode=all 不指定 AllocTo：运行时分不出去，必须发布期就拦住。
//   - RetainBPS 越界：负数等于凭空造钱，超过 10000 等于发出比领到的还多。
func (ix *Index) detectPoolRules() {
	for _, r := range ix.allRules() {
		if r.Phase == domain.PhaseDistribute && r.Action.Kind == domain.ActDistributePool {
			if r.Action.DistMode == "" {
				ix.Conflicts = append(ix.Conflicts, fmt.Sprintf(
					"[pool] %s 是池分发规则但未配置 DistMode（equal / all / weight 三选一）", r.ID))
			}
			if r.Action.RetainBPS < 0 || r.Action.RetainBPS > money.BPS {
				ix.Conflicts = append(ix.Conflicts, fmt.Sprintf(
					"[pool] %s 的 RetainBPS=%d 越界（0~%d）", r.ID, r.Action.RetainBPS, money.BPS))
			}
		}
		if r.Action.DistMode == "" {
			continue
		}
		switch r.Action.DistMode {
		case domain.DistEqual, domain.DistAll, domain.DistWeight:
		default:
			ix.Conflicts = append(ix.Conflicts, fmt.Sprintf(
				"[pool] %s 分发方式 %q 未定义（只允许 equal / all / weight）", r.ID, r.Action.DistMode))
			continue
		}
		if r.Phase != domain.PhaseDistribute {
			ix.Conflicts = append(ix.Conflicts, fmt.Sprintf(
				"[pool] %s 分发方式 %s 只能配在 PhaseDistribute（当前 %s）", r.ID, r.Action.DistMode, r.Phase))
		}
		if r.Action.DistMode == domain.DistAll && r.Action.AllocTo == "" {
			ix.Conflicts = append(ix.Conflicts, fmt.Sprintf(
				"[pool] %s 是 DistMode=all 但未指定 AllocTo，运行时分不出去", r.ID))
		}
		if r.Action.DistMode == domain.DistWeight && len(r.Action.WeightTable) == 0 {
			ix.Conflicts = append(ix.Conflicts, fmt.Sprintf(
				"[pool] %s 是 DistMode=weight 但未配置 WeightTable", r.ID))
		}
	}
}

func (ix *Index) detectGaps(cat Catalog) {
	// 静态检查只看 scope 是否覆盖，不看生效期（生效期由运行时按 period 判定）。
	// 用 Overlaps 而不是 Matches：目录里没有部门，"按部门配的规则"不能被误判成缺口。
	for _, l := range cat.Lines {
		if !ix.covered(domain.PhaseFund, domain.Scope{Lines: []domain.ProductLineID{l}}) {
			ix.Gaps = append(ix.Gaps, fmt.Sprintf("[fund] 产品线 %s 无入池规则", l))
		}
	}
	for _, l := range cat.Lines {
		for _, r := range cat.Roles {
			if !ix.covered(domain.PhaseDirect, domain.Scope{Lines: []domain.ProductLineID{l}, Roles: []domain.RoleID{r}}) {
				ix.Gaps = append(ix.Gaps, fmt.Sprintf("[direct] 产品线 %s / 角色 %s 无佣金规则", l, r))
			}
		}
	}
	for _, r := range cat.Roles {
		for _, k := range []domain.KPI{domain.KPIContract, domain.KPIRevenue} {
			if !ix.covered(domain.PhaseAttain, domain.Scope{Roles: []domain.RoleID{r}, KPIs: []domain.KPI{k}}) {
				ix.Gaps = append(ix.Gaps, fmt.Sprintf("[attain] 角色 %s / 口径 %s 无档位表，按倍数 1.0 计算", r, k))
			}
		}
	}
	// 有领取规则的目标池却没配分发规则：领了发不出去，只能全额结转。
	// 这是规则与规则之间的关系，发布期就能判定，不需要等运行时。
	seen := map[domain.PoolID]bool{}
	for _, r := range ix.byPhase[domain.PhaseFund] {
		pid := domain.PoolID(r.Action.Target)
		if pid == "" || seen[pid] {
			continue
		}
		seen[pid] = true
		if !ix.covered(domain.PhaseDistribute, domain.Scope{Pools: []domain.PoolID{pid}}) {
			ix.Gaps = append(ix.Gaps, fmt.Sprintf("[pool] 池 %s 无分发规则，领取额全额结转", pid))
		}
	}
	// 因子缺失只告警不报错：不用时间/产品因子的公司是合法配置，但"漏配一季"
	// 会让整季少算，且结果上看不出异常（金额偏小但没有 0）。
	for _, q := range cat.Quarters {
		if !ix.coveredKind(domain.PhaseFactor, domain.ActTimeFactor, domain.Scope{Quarters: []domain.Quarter{q}}) {
			ix.Gaps = append(ix.Gaps, fmt.Sprintf("[factor] 季度 %s 无时间因子规则，按 1.0 计算", q))
		}
	}
	for _, l := range cat.Lines {
		if !ix.coveredKind(domain.PhaseFactor, domain.ActProductFactor, domain.Scope{Lines: []domain.ProductLineID{l}}) {
			ix.Gaps = append(ix.Gaps, fmt.Sprintf("[factor] 产品线 %s 无产品因子规则，按 1.0 计算", l))
		}
	}
}

// coveredKind 与 covered 相同，但只看指定 Kind 的规则：
// 产品因子规则的通配 Scope 会与"季度 Q4"相交，不分 Kind 就会把时间因子的缺口盖掉。
func (ix *Index) coveredKind(phase domain.Phase, kind domain.ActionKind, want domain.Scope) bool {
	for _, r := range ix.byPhase[phase] {
		if r.Action.Kind != kind {
			continue
		}
		if r.Scope.Overlaps(want) {
			return true
		}
	}
	return false
}

// covered 判断是否存在一条规则，其作用域与 want 描述的场景有交集。
func (ix *Index) covered(phase domain.Phase, want domain.Scope) bool {
	for _, r := range ix.byPhase[phase] {
		if r.Scope.Overlaps(want) {
			return true
		}
	}
	return false
}

// MatrixLines 输出发布期人工复核用的矩阵视图。
func (ix *Index) MatrixLines(cat Catalog) []string {
	out := make([]string, 0, len(cat.Lines))
	for _, l := range cat.Lines {
		for _, r := range cat.Roles {
			var ids []string
			for _, ru := range ix.byPhase[domain.PhaseDirect] {
				if ru.Scope.Overlaps(domain.Scope{Lines: []domain.ProductLineID{l}, Roles: []domain.RoleID{r}}) {
					ids = append(ids, string(ru.ID))
				}
			}
			mark := "OK "
			if len(ids) == 0 {
				mark = "GAP"
			} else if len(ids) > 1 {
				mark = "MUL"
			}
			out = append(out, fmt.Sprintf("%s  %-6s x %-12s -> %s", mark, l, r, strings.Join(ids, ", ")))
		}
	}
	return out
}
