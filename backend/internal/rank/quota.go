// Package rank 放的是 **Feed 的配额槽位算法**，没有别的。
//
// 为什么单独一个包而不是留在 internal/feed 里：
// P2 的离线评测（internal/evalset）必须与线上用**同一份**配额规则，
// 否则"离线评测说新排序更好"只证明了评测里那个近似实现更好，
// 而线上跑的是另一套代码——那正是离线评测最容易变成自欺的地方。
//
// 这个包刻意只依赖标准库，也不认识 video/feed/evalset 的任何类型：
// 线上路径（feed）与离线路径（evalset）都能 import 它而不产生循环，
// 而"槽位几何"这一条性质在两条路径上是**同一个函数**。
//
// 本包不做的事（它们留在各自的包，因为它们依赖各自的领域类型）：
//   - 决定候选怎么取（线上是 ZSET 冷热拼接 + 向量扫描，离线是快照）；
//   - 决定候选怎么排序（线上是余弦分，离线是同一套余弦分）；
//   - 决定最终列表怎么拼（各自的元素类型不同）。
package rank

// 槽位落点。这两个常量是 P2 的"用户可感知"参数：
//
//   - 语义位从第 3 位开始、每 3 位一个。集中放在开头会让首屏看起来像
//     换了一个产品（用户对第一条最敏感），而 P2 的目标是"补上原有排序
//     捞不到的那部分"，不是替换它。
//   - 探索位从第 6 位开始。探索内容的相关性天然偏低（它的定义就是
//     "还没有互动"），放在最前面会直接拉低首屏观感，放在第 6 位之后
//     已经足够让创作者拿到曝光。
const (
	SemanticSlotOffset = 2
	SemanticSlotStride = 3
	ExploreSlotOffset  = 5
	ExploreSlotStride  = 10
)

// Channel 是最终列表某一位的提供方。
type Channel uint8

const (
	// Base 表示这一位来自**基础页**（既有的时序排序）。
	//
	// 零值是 Base 是有意的：配额全关时 slots 里全是零值，
	// 而零值恰好就是"这一页完全交给既有排序"——这正是 P2 §5.1
	// 要求的"与基准线逐字节一致"。
	Base Channel = iota
	// Semantic 表示这一位留给语义召回。
	Semantic
	// Explore 表示这一位留给冷启动探索。
	Explore
)

// QuotaPlan 是"最终列表里有多少个位置留给哪一路"。
type QuotaPlan struct {
	Semantic int
	Explore  int
}

// Enabled 报告是否有任何一路被启用。
//
// 调用方用它做**零开销判断**：两者都是 0 时，整条语义路（用户画像、
// 向量读取、相似度计算、探索查询）一次都不执行。把这件事写成
// "跑到一半再早退"会让"配额 0 = 零开销"变成靠人自觉维持的性质。
func (p QuotaPlan) Enabled() bool { return p.Semantic > 0 || p.Explore > 0 }

// EvalQuota 是 offline 评测里用的配额（比例为 0 时返回零值计划）。
func EvalQuota(limit int, recallQuota, exploreQuota float64) QuotaPlan {
	return Plan(limit, recallQuota, exploreQuota)
}

// Plan 把配额比例换算成位置数。
//
// 两条纪律写在这里而不是让调用方各自处理：
//
//  1. **配额位不得吃满整页**。吃满等于把既有排序整体替换掉
//     （P2 §3 明确禁止），而且用户会立刻察觉"这不是原来那个 Feed 了"。
//     留至少一个基础位，原有排序就始终还在页面上——这也是"可被开关
//     切回"的技术前提。
//  2. **非零配额至少给 1 个位置**。否则 limit=10、quota=0.05 会被取整成 0，
//     配置里写了配额却毫无效果，而现象是"怎么调都没变化"。
//     反过来，配额为 0 时必须真的是 0。
func Plan(limit int, recallQuota, exploreQuota float64) QuotaPlan {
	if limit <= 0 {
		return QuotaPlan{}
	}
	var plan QuotaPlan
	if recallQuota > 0 {
		plan.Semantic = roundQuota(limit, recallQuota)
	}
	if exploreQuota > 0 {
		plan.Explore = roundQuota(limit, exploreQuota)
	}

	// 两道收缩，顺序不能反：
	//
	//  1. **语义位先让路**：语义是主特征，但它不能吃满整页。
	//  2. **空间不够时探索让路**：P2 §4.5 的兜底优先于"语义位多一个"，
	//     因为"新内容永不曝光"是事故，而"语义位少一个"只是效果差一点。
	//     但兜底同样不能让整页变成探索内容。
	if plan.Semantic >= limit {
		plan.Semantic = limit - 1
	}
	if plan.Semantic < 0 {
		plan.Semantic = 0
	}
	if plan.Semantic+plan.Explore >= limit {
		plan.Explore = limit - plan.Semantic - 1
	}
	if plan.Explore < 0 {
		plan.Explore = 0
	}
	return plan
}

// roundQuota 把比例换算成位置数，非零配额至少给 1 个位置。
func roundQuota(limit int, quota float64) int {
	if quota <= 0 || limit <= 0 {
		return 0
	}
	n := int(quota*float64(limit) + 0.5)
	if n == 0 {
		n = 1
	}
	if n > limit {
		n = limit
	}
	return n
}

// Slots 返回长度为 limit 的槽位表：slots[i] 表示最终列表第 i 位由哪一路提供。
//
// 它是**唯一的**槽位几何定义。线上（feed.fuseChannels）与离线评测
// （evalset 的 P2 排序器）都调用它，因此"配额位落在第几位"这件事
// 在两条路径上不可能漂移。
//
// 位置冲突的处理：语义位与探索位可能落在同一个下标（limit 很小时），
// 那时探索位向后找第一个空位；找不到就少一个探索位。
// 宁可少一个探索位，也不挤占语义位或基础位——后两者的语义更明确。
func Slots(limit int, plan QuotaPlan) []Channel {
	if limit <= 0 {
		return nil
	}
	slots := make([]Channel, limit)
	if plan.Semantic > limit {
		plan.Semantic = limit
	}
	if plan.Explore > limit-plan.Semantic {
		plan.Explore = limit - plan.Semantic
	}
	for i := 0; i < plan.Semantic; i++ {
		pos := SemanticSlotOffset + i*SemanticSlotStride
		if pos >= limit {
			// 位置用完了：宁可少给一个语义位，也不挤占后面的基础位
			// （挤占会让"配额"变成"整体替换"）。
			break
		}
		slots[pos] = Semantic
	}
	for i := 0; i < plan.Explore; i++ {
		pos := ExploreSlotOffset + i*ExploreSlotStride
		for pos < limit && slots[pos] != Base {
			pos++
		}
		if pos >= limit {
			break
		}
		slots[pos] = Explore
	}
	return slots
}
