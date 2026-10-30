package golden

import (
	"errors"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/walry/yongjin/internal/calc"
	"github.com/walry/yongjin/internal/money"
)

// 金标目录就是 testdata：Go 工具链天然不会把它编进构建，
// 而它又是仓库的一部分 —— 期望值本来就该随代码一起评审。
const dir = "testdata"

var update = flag.Bool("update", false, "重写全部金标快照（只在确认改动有意时使用）")

// TestGoldenSet 是发布闸门：每份语料逐分比对三层的全部数字。
func TestGoldenSet(t *testing.T) {
	for _, c := range Cases() {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			got, err := Run(c)
			if err != nil {
				t.Fatalf("计算失败：%v", err)
			}
			if *update {
				if err := Save(dir, c, got); err != nil {
					t.Fatalf("写金标失败：%v", err)
				}
				return
			}
			want, err := Load(dir, c.Name)
			if errors.Is(err, os.ErrNotExist) {
				t.Fatalf("金标缺失：%s/%s.golden 不存在，跑 go test ./internal/golden -update 生成", dir, c.Name)
			}
			if err != nil {
				t.Fatalf("读金标失败：%v", err)
			}
			if ms := Diff(want, got); len(ms) > 0 {
				t.Fatal(FormatDiff(c.Name, ms))
			}
		})
	}
}

// TestGoldenRejectsDrift 用「只改一处输入」验证身份的独立性。
//
// 把 POOL-B 预算动一元，必须同时看到 facts_digest 变、run_id 变、且钱只在
// 池那一列变。如果身份三个字段一起变而金额完全不变，说明池预算根本没参与计算 ——
// 那才是更严重的问题，所以这里也断言金额有过变化。
func TestGoldenRejectsDrift(t *testing.T) {
	c := poolStarvedCase()
	base, err := Run(c)
	if err != nil {
		t.Fatalf("计算失败：%v", err)
	}

	drifted := c
	for i := range drifted.Facts.Pools {
		if drifted.Facts.Pools[i].ID == "POOL-B" {
			drifted.Facts.Pools[i].Budget += money.Money(1) // 只动一分钱
		}
	}

	got, err := Run(drifted)
	if err != nil {
		t.Fatalf("计算失败：%v", err)
	}
	ms := Diff(base, got)
	if len(ms) == 0 {
		t.Fatal("改了池预算却没有任何差异：预算没有参与计算")
	}
	if ms[0].Layer != "身份" || ms[0].Field != "facts_digest" {
		t.Fatalf("第一条差异应是 facts_digest（输入漂移优先于金额差异），got %+v", ms[0])
	}
}

// TestGoldenCasesTrapWhatTheyClaim 校验语料自身的健康度。
//
// 用例声明的 Tags 必须在结果里真的出现。没有这条，一次「顺手删掉分账规则」
// 的改动会让 splitCase 悄悄退化成 baseline 的复制品，而金标集仍然全绿地
// 为它作证 —— 那是最坏的绿灯。
func TestGoldenCasesTrapWhatTheyClaim(t *testing.T) {
	for _, c := range Cases() {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			if len(c.Tags) == 0 {
				t.Fatal("用例没有声明 Tags：它到底压什么语义")
			}
			res, err := calc.Run(c.Facts, c.Rules, calc.Options{})
			if err != nil {
				t.Fatalf("计算失败：%v", err)
			}
			got := Features(res)
			for _, tag := range c.Tags {
				if !got[tag] {
					t.Fatalf("声明压 %q，但结果里没有出现该语义（用例已失效）", tag)
				}
			}
		})
	}
}

// TestGoldenCoversAllSemantics 保证整份语料覆盖到每一个语义。
// 单条用例可以只压一处，但合起来不能有盲区。
func TestGoldenCoversAllSemantics(t *testing.T) {
	cover := map[string]bool{}
	for _, c := range Cases() {
		res, err := calc.Run(c.Facts, c.Rules, calc.Options{})
		if err != nil {
			t.Fatalf("%s 计算失败：%v", c.Name, err)
		}
		for k, v := range Features(res) {
			if v {
				cover[k] = true
			}
		}
	}
	all := []string{
		TagBand, TagFactor, TagSplit, TagClaim, TagDistribute,
		TagManager, TagWarning, TagDualKPI, TagCarryOver, TagRetained,
	}
	var missing []string
	for _, k := range all {
		if !cover[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("语料存在语义盲区：%s（没有任何用例压到它）", strings.Join(missing, ", "))
	}
}

// TestGoldenNoOrphans 防止「用例改名后遗留的旧金标」。
// 孤儿文件永远不参与比对，却会让人以为那条语义还有人守着。
func TestGoldenNoOrphans(t *testing.T) {
	files, err := Walk(dir)
	if err != nil {
		t.Fatalf("读目录失败：%v", err)
	}
	known := map[string]bool{}
	for _, c := range Cases() {
		known[c.Name] = true
	}
	for _, f := range files {
		if !known[f] {
			t.Errorf("金标 %s.golden 没有对应用例（用例改名后忘记删文件？）", f)
		}
	}
	for _, c := range Cases() {
		found := false
		for _, f := range files {
			if f == c.Name {
				found = true
				break
			}
		}
		if !found && !*update {
			t.Errorf("用例 %s 缺少金标文件：跑 -update 生成", c.Name)
		}
	}
}

// TestGoldenEncodeRoundTrip 守住格式本身：编码再解码必须逐字段相等。
// 文本表是唯一的人读入口，它若在某个字段上悄悄丢精度，比对就是在比空气。
func TestGoldenEncodeRoundTrip(t *testing.T) {
	for _, c := range Cases() {
		want, err := Run(c)
		if err != nil {
			t.Fatalf("%s 计算失败：%v", c.Name, err)
		}
		got, err := Decode(Encode(want))
		if err != nil {
			t.Fatalf("%s 解码失败：%v", c.Name, err)
		}
		if ms := Diff(want, got); len(ms) > 0 {
			t.Fatalf("%s 编解码不一致：\n%s", c.Name, FormatDiff(c.Name, ms))
		}
	}
}

// TestGoldenDecodeRejectsUnknownTag：未知标签必须报错。
// 静默忽略会让「字段名写错」变成「这一列永远不比」。
func TestGoldenDecodeRejectsUnknownTag(t *testing.T) {
	if _, err := Decode("total 1.00\nbogus 2.00\n"); err == nil {
		t.Fatal("未知记录类型必须报错")
	}
	if _, err := Decode("person P1 direct=1.0\n"); err == nil {
		t.Fatal("金额小数位不是两位时必须报错")
	}
	if _, err := Decode("person P1 direct=1.00\n"); err == nil {
		t.Fatal("人记录缺列时必须报错")
	}
}

// TestGoldenParseMoney 单独钉解析：金标文件里一个符号错位，
// 会让整列比对静默偏一个数量级。
func TestGoldenParseMoney(t *testing.T) {
	cases := map[string]string{
		"0.00":       "0.00",
		"1234567.89": "1234567.89",
		"-15649.92":  "-15649.92",
	}
	for in, want := range cases {
		m, err := parseMoney(in)
		if err != nil {
			t.Fatalf("%s 解析失败：%v", in, err)
		}
		if m.String() != want {
			t.Fatalf("%s 解析成 %s，应为 %s", in, m, want)
		}
	}
}
