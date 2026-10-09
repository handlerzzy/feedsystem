package rank

import "testing"

// 本文件是**配额语义的唯一测试落点**：线上（feed）与离线评测（evalset）
// 共用同一个 Plan/Slots，所以这里断言的性质同时保护两条路径。

func TestPlanQuotas(t *testing.T) {
	cases := []struct {
		name    string
		limit   int
		recall  float64
		explore float64
		want    QuotaPlan
	}{
		{name: "全关", limit: 20, want: QuotaPlan{}},
		{name: "默认设计取值", limit: 20, recall: 0.3, explore: 0.1, want: QuotaPlan{Semantic: 6, Explore: 2}},
		{name: "小页四舍五入", limit: 10, recall: 0.3, explore: 0.1, want: QuotaPlan{Semantic: 3, Explore: 1}},
		{name: "非零配额至少一位", limit: 10, recall: 0.01, want: QuotaPlan{Semantic: 1}},
		{name: "只开探索", limit: 10, explore: 0.2, want: QuotaPlan{Explore: 2}},
		{name: "limit 为 0", limit: 0, recall: 0.3, explore: 0.1, want: QuotaPlan{}},
		{name: "limit 为负", limit: -3, recall: 0.3, want: QuotaPlan{}},
		// recall_quota=1 想独占整页：必须被收缩到 limit-1。
		{name: "配额不得吃满整页", limit: 4, recall: 1, explore: 1, want: QuotaPlan{Semantic: 3}},
		{name: "两者相加超限时探索让路", limit: 10, recall: 0.9, explore: 0.9, want: QuotaPlan{Semantic: 9}},
		// limit=3：语义、探索各 1.5 位，四舍五入后都想拿 2 位。
		// 语义优先（主特征），探索被收缩到 0 —— 页面上永远留至少一个基础位。
		{name: "刚好差一位", limit: 3, recall: 0.5, explore: 0.5, want: QuotaPlan{Semantic: 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Plan(tc.limit, tc.recall, tc.explore)
			if got != tc.want {
				t.Fatalf("Plan(%d, %v, %v) = %+v, want %+v", tc.limit, tc.recall, tc.explore, got, tc.want)
			}
			if tc.limit > 0 && got.Semantic+got.Explore >= tc.limit {
				t.Fatalf("配额位 %d+%d 吃满了整页（limit=%d）：既有排序会被整体替换掉",
					got.Semantic, got.Explore, tc.limit)
			}
			if (tc.recall > 0 || tc.explore > 0) && got == (QuotaPlan{}) && tc.limit > 0 {
				t.Fatalf("配置里写了非零配额却一个位置都没留：现象是'怎么调都没变化'")
			}
		})
	}
}

func TestPlanEnabled(t *testing.T) {
	if (QuotaPlan{}).Enabled() {
		t.Fatal("零值计划必须是关闭的：它是'配额全关 = 零开销'的判断依据")
	}
	if !(QuotaPlan{Semantic: 1}).Enabled() || !(QuotaPlan{Explore: 1}).Enabled() {
		t.Fatal("任一路有位置就必须报告启用")
	}
}

func TestSlotsLayout(t *testing.T) {
	slots := Slots(10, QuotaPlan{Semantic: 3, Explore: 1})
	// 语义位从下标 2 开始每 3 位一个：2、5、8；
	// 探索位从下标 5 开始，而 5 已被语义位占用 -> 向后找第一个空位（6）。
	// 因此实际布局是 2(S)、5(S)、6(E)、8(S)。
	want := []Channel{Base, Base, Semantic, Base, Base, Semantic, Explore, Base, Semantic, Base}
	if len(slots) != len(want) {
		t.Fatalf("槽位长度 = %d, want %d", len(slots), len(want))
	}
	for i := range want {
		if slots[i] != want[i] {
			t.Fatalf("槽位[%d] = %v, want %v\n got %v\nwant %v", i, slots[i], want[i], slots, want)
		}
	}
}

func TestSlotsAllBaseWhenNoQuota(t *testing.T) {
	// 这一条是 P2 §5.1 的实现依据：配额全关时槽位表全是零值（Base），
	// 于是装配循环只走基础页通道 -> 输出与基准线逐字节一致。
	for _, c := range Slots(7, QuotaPlan{}) {
		if c != Base {
			t.Fatalf("配额全关时出现了非基础位: %v", Slots(7, QuotaPlan{}))
		}
	}
}

func TestSlotsSmallLimitNeverOverflows(t *testing.T) {
	// limit 很小、配额很大时，语义位与探索位会互相撞车。
	// 无论怎么撞，槽位表长度必须等于 limit，且不能全是配额位。
	for limit := 1; limit <= 12; limit++ {
		for _, plan := range []QuotaPlan{
			{Semantic: limit, Explore: limit},
			{Semantic: 1, Explore: limit},
			{Semantic: limit, Explore: 1},
		} {
			slots := Slots(limit, plan)
			if len(slots) != limit {
				t.Fatalf("Slots(%d, %+v) 长度 = %d", limit, plan, len(slots))
			}
			base := 0
			for _, c := range slots {
				if c == Base {
					base++
				}
			}
			if base == 0 {
				t.Fatalf("Slots(%d, %+v) 里没有基础位：既有排序被整体替换了", limit, plan)
			}
		}
	}
}

func TestSlotsZeroLimit(t *testing.T) {
	if got := Slots(0, QuotaPlan{Semantic: 3}); got != nil {
		t.Fatalf("limit=0 时应当返回 nil, got %v", got)
	}
}

func TestEvalQuotaMatchesPlan(t *testing.T) {
	// 离线评测的入口必须与线上完全同构，否则"离线说更好"没有意义。
	if EvalQuota(20, 0.3, 0.1) != Plan(20, 0.3, 0.1) {
		t.Fatal("EvalQuota 与 Plan 的结果不一致：离线评测与线上用了两套配额规则")
	}
}
