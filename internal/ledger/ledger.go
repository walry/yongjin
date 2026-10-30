// Package ledger 是追加式台账：已锁定期间不可改写，规则变更只产生调整分录。
package ledger

import (
	"fmt"
	"sort"
	"strings"

	"github.com/walry/yongjin/internal/calc"
	"github.com/walry/yongjin/internal/domain"
	"github.com/walry/yongjin/internal/money"
)

type Status string

const (
	StatusOpen       Status = "open"
	StatusCalculated Status = "calculated"
	StatusLocked     Status = "locked"
)

type Entry struct {
	ID        string
	Period    domain.Period
	Person    domain.PersonID
	Kind      string // settlement / adjustment
	Amount    money.Money
	CalcRunID string
	AdjustOf  string
	// Cause 说明这次重算是哪一维输入变了，形如 `rules:v1->v2 facts:3f2a91c0->8b7d4e12`。
	//
	// 它由 Recalculate 比对新旧结果的四要素推出，不接受调用方自报：自报的理由只会
	// 和实际入账的数字对不上，而台账的价值恰恰在于「事后不问人也能复核」。判据与
	// CalcRunID 完全一致，所以「Cause 非空」与「CalcRunID 变化」永远同时成立。
	//
	// 更细的归因（改了哪条规则、哪个池预算）不在这里，要拿 Cause 里的
	// ruleSetVersion / factsDigest 回快照上查 —— 台账只负责「哪一维变了」这个事实。
	Cause string
}

type Book struct {
	entries []Entry
	status  map[domain.Period]Status
	seq     int
}

func New() *Book {
	return &Book{status: map[domain.Period]Status{}}
}

func (b *Book) Status(p domain.Period) Status {
	if s, ok := b.status[p]; ok {
		return s
	}
	return StatusOpen
}

func (b *Book) Lock(p domain.Period) { b.status[p] = StatusLocked }

func (b *Book) Entries() []Entry { return append([]Entry(nil), b.entries...) }

func (b *Book) append(period domain.Period, person domain.PersonID, kind string, amt money.Money, runID, adjustOf, cause string) Entry {
	b.seq++
	e := Entry{ID: fmt.Sprintf("E-%05d", b.seq), Period: period, Person: person, Kind: kind, Amount: amt, CalcRunID: runID, AdjustOf: adjustOf, Cause: cause}
	b.entries = append(b.entries, e)
	return e
}

// Post 写入首次结算分录。已锁定期间拒绝写入。
//
// 未锁定的 Calculated 期间再次 Post 是「重算覆盖草稿」（ARCHITECTURE §7.2 的状态机），
// 因此要先丢掉该期间已有的 settlement：直接追加会让同一期间出现两套结算分录，
// 账面凭空翻倍，而且两套各自指向不同 CalcRunID，事后无从判断哪一套是最终稿。
// adjustment 不在此列 —— 它只可能产生于锁定之后，见 Recalculate。
func (b *Book) Post(res calc.Result) ([]Entry, error) {
	if b.Status(res.Period) == StatusLocked {
		return nil, fmt.Errorf("期间 %s 已锁定，只能走调整单", res.Period)
	}
	if b.Status(res.Period) == StatusCalculated {
		kept := b.entries[:0]
		for _, e := range b.entries {
			if e.Period == res.Period && e.Kind == "settlement" {
				continue
			}
			kept = append(kept, e)
		}
		b.entries = kept
	}
	var out []Entry
	for _, p := range res.Persons {
		out = append(out, b.append(res.Period, p.Person, "settlement", p.Total, res.CalcRunID, "", ""))
	}
	b.status[res.Period] = StatusCalculated
	return out, nil
}

// Recalculate 对已结算期间做历史重算：只生成差额调整分录，与新旧计算单双向关联。
// 守恒断言：Σdelta == Σnew - Σold。
func (b *Book) Recalculate(oldRes, newRes calc.Result) ([]Entry, error) {
	if oldRes.Period != newRes.Period {
		return nil, fmt.Errorf("期间不一致: %s vs %s", oldRes.Period, newRes.Period)
	}
	// 重算差额只对已锁定期间有意义：未结算时直接 Post 就是重算，未锁定时 Post 覆盖
	// 草稿。若在这里放行，账面会从「只有一批 adjustment、没有 settlement」开始，
	// PeriodTotal 等于差额而不是新合计，此后无论怎么调都补不回来。
	if st := b.Status(oldRes.Period); st != StatusLocked {
		return nil, fmt.Errorf("期间 %s 状态为 %s：重算差额只对已锁定期间有意义，未锁定直接 Post 即可", oldRes.Period, st)
	}

	deltas := map[domain.PersonID]money.Money{}
	add := func(p domain.PersonID, d money.Money) {
		deltas[p] += d
	}
	for _, p := range newRes.Persons {
		add(p.Person, p.Total)
	}
	for _, p := range oldRes.Persons {
		add(p.Person, -p.Total)
	}
	// 按人 ID 排序生成分录：差额本身与顺序无关，但入账顺序必须可复现，
	// 否则同一份重算两次会写出两套顺序不同的分录编号。
	order := make([]domain.PersonID, 0, len(deltas))
	for p := range deltas {
		order = append(order, p)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })

	var sumDelta, sumNew, sumOld money.Money
	for _, d := range deltas {
		sumDelta += d
	}
	for _, p := range newRes.Persons {
		sumNew += p.Total
	}
	for _, p := range oldRes.Persons {
		sumOld += p.Total
	}
	if sumDelta != sumNew-sumOld {
		return nil, fmt.Errorf("重算守恒失败: delta=%s new-old=%s", sumDelta, sumNew-sumOld)
	}

	cause := causeOf(oldRes, newRes)
	if cause == "" {
		// 四要素全同却算出了不同合计：这不是「重算」，是引擎不确定（I9 的逆否）。
		// 入账等于把一次不可复现的差异固化进台账，所以宁可拒。
		for _, p := range order {
			if deltas[p] != 0 {
				return nil, fmt.Errorf("重算差额非零但四要素未变：%s 差 %s（引擎不确定）", p, deltas[p])
			}
		}
	}
	var out []Entry
	for _, p := range order {
		d := deltas[p]
		if d == 0 {
			continue
		}
		out = append(out, b.append(newRes.Period, p, "adjustment", d, newRes.CalcRunID, oldRes.CalcRunID, cause))
	}
	return out, nil
}

// causeOf 把「哪一维输入变了」写成一句可入账的理由。
//
// 判据与 calc.CalcRunID 的输入完全一致（period 已校验相同），所以只要重算真的
// 换了口径，Cause 就不为空；反过来 Cause 为空而差额非零，说明结果发生了变化却没有
// 任何输入变化 —— 那是引擎不确定，必须当 defect 追。
func causeOf(oldRes, newRes calc.Result) string {
	var parts []string
	if oldRes.RuleSetVersion != newRes.RuleSetVersion {
		parts = append(parts, fmt.Sprintf("rules:%s->%s", oldRes.RuleSetVersion, newRes.RuleSetVersion))
	}
	if oldRes.FactsDigest != newRes.FactsDigest {
		parts = append(parts, fmt.Sprintf("facts:%s->%s", shortDigest(oldRes.FactsDigest), shortDigest(newRes.FactsDigest)))
	}
	if oldRes.EngineVersion != newRes.EngineVersion {
		parts = append(parts, fmt.Sprintf("engine:%s->%s", oldRes.EngineVersion, newRes.EngineVersion))
	}
	return strings.Join(parts, " ")
}

// shortDigest 只截前 8 位：摘要的用途是区分「变了没有」，不是给人读全文。
// 截断后仍然逐条可比，写进分录也不会把台账撑爆。
func shortDigest(d string) string {
	if len(d) <= 8 {
		return d
	}
	return d[:8]
}

// Balance 返回某人在某期间的账面合计（含调整）。
func (b *Book) Balance(period domain.Period, person domain.PersonID) money.Money {
	var s money.Money
	for _, e := range b.entries {
		if e.Period == period && e.Person == person {
			s += e.Amount
		}
	}
	return s
}

func (b *Book) PeriodTotal(period domain.Period) money.Money {
	var s money.Money
	for _, e := range b.entries {
		if e.Period == period {
			s += e.Amount
		}
	}
	return s
}
