package calc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/walry/yongjin/internal/domain"
)

// Digest 对事实快照做稳定摘要，用于幂等键。
//
// 只靠 encoding/json 的 map 键排序是不够的：事实里有四处切片，其顺序只反映录入
// 顺序，不携带业务含义。顺序一变摘要就变，同事实同规则会被判成"需要重算"，幂等
// 失效。因此摘要前先 canonical 规范化：排序所有切片，让"同事实"真的同摘要。
func Digest(f domain.Facts) (string, error) {
	b, err := json.Marshal(canonical(f))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func runID(period domain.Period, digest, ruleVer, engineVer string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s", period, digest, ruleVer, engineVer)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// canonical 返回切片全部有序的深拷贝。原始事实不被修改（Facts 只读语义）。
// 排列键一律用业务标识而非下标：订单按 ID，行按 ID，协作人按 (人, 角色)，
// 关系按 (From, To, Kind)，人员辖区与区域成员按 ID。
func canonical(f domain.Facts) domain.Facts {
	out := f

	out.Orders = make([]domain.Order, len(f.Orders))
	for i, o := range f.Orders {
		o.Participants = append([]domain.Participant(nil), o.Participants...)
		sort.Slice(o.Participants, func(a, b int) bool {
			if o.Participants[a].Person != o.Participants[b].Person {
				return o.Participants[a].Person < o.Participants[b].Person
			}
			return o.Participants[a].Role < o.Participants[b].Role
		})
		o.Lines = append([]domain.OrderLine(nil), o.Lines...)
		sort.Slice(o.Lines, func(a, b int) bool { return o.Lines[a].ID < o.Lines[b].ID })
		out.Orders[i] = o
	}
	sort.Slice(out.Orders, func(i, j int) bool { return out.Orders[i].ID < out.Orders[j].ID })

	out.Relations = append([]domain.Relation(nil), f.Relations...)
	sort.Slice(out.Relations, func(i, j int) bool {
		a, b := out.Relations[i], out.Relations[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		return a.Kind < b.Kind
	})

	out.Targets = append([]domain.Target(nil), f.Targets...)
	sort.Slice(out.Targets, func(i, j int) bool {
		a, b := out.Targets[i], out.Targets[j]
		if a.Person != b.Person {
			return a.Person < b.Person
		}
		return a.Period < b.Period
	})

	out.Pools = append([]domain.PoolDef(nil), f.Pools...)
	sort.Slice(out.Pools, func(i, j int) bool { return out.Pools[i].ID < out.Pools[j].ID })

	if f.Persons != nil {
		out.Persons = make(map[domain.PersonID]domain.Person, len(f.Persons))
		for k, p := range f.Persons {
			p.Territories = append([]domain.TerritoryID(nil), p.Territories...)
			sort.Slice(p.Territories, func(a, b int) bool { return p.Territories[a] < p.Territories[b] })
			out.Persons[k] = p
		}
	}
	if f.Territories != nil {
		out.Territories = make(map[domain.TerritoryID]domain.Territory, len(f.Territories))
		for k, t := range f.Territories {
			t.Members = append([]domain.PersonID(nil), t.Members...)
			sort.Slice(t.Members, func(a, b int) bool { return t.Members[a] < t.Members[b] })
			out.Territories[k] = t
		}
	}
	return out
}
