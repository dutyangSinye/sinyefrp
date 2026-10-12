import { ROBOT_TYPE_LABEL, ROBOT_TYPE_NONE, ROBOT_TYPES } from '@/api/types'

// 机器人类型的分组视图（01-prd F20）。
//
// 台账页与客户端页都要求「按机器人类型归拢展示」，两处的排序规则、未分类的
// 归属、合并单元格的算法完全一样。这些规则只放一份在这里 —— 各写一份的话，
// 迟早出现「台账里未分类排在最后、客户端里排在中间」这种同一份数据两种次序
// 的不一致，而且没人会在测试里发现。
//
// 值域权威仍是 Go 端 internal/pack/spec.go（AllRobotTypes）；这里只做展示与分组。

/** 筛选条的「全部」取值。 */
export const ROBOT_FILTER_ALL = 'all'

/**
 * 筛选条的「未分类」取值。
 *
 * 空串本身是合法的类型取值（未分类），所以不能拿它当筛选值 ——
 * 那样「筛选未分类」与「没有筛选」在代码里是同一个值，必然出错。
 */
export const ROBOT_FILTER_NONE = 'none'

/** 一条记录归入哪个分组：机器人类型取值，空串表示未分类。 */
export type RobotKey = string

/**
 * 分组的排序权重。
 *
 * 具体类型按 ROBOT_TYPES 的顺序 —— 与生成端的下拉顺序一致，管理员在
 * 「生成包」和「看台账」两处看到的是同一个次序，不用重新适应。
 * 未分类排在最后：它是兜底归类，不该占住列表开头。
 * 未知取值排在未分类之前且**保留原值**：宁可让它显眼地落在末尾附近，
 * 也不要静默丢弃 —— 丢弃会让一台客户端在界面上凭空消失。
 *
 * 注意这套次序只在「分组视图」里生效，不改变后端返回的原始顺序。
 */
export function robotTypeRank(v: string | null | undefined): number {
  const k = (v ?? '').trim()
  if (!k) return ROBOT_TYPES.length + 1
  const i = ROBOT_TYPES.indexOf(k)
  return i >= 0 ? i : ROBOT_TYPES.length
}

/** 把某个类型的取值翻成中文标签；空值落到「未分类」，未知值原样返回。 */
export function robotLabel(v: string | null | undefined): string {
  const k = (v ?? '').trim()
  if (!k) return ROBOT_TYPE_NONE
  return ROBOT_TYPE_LABEL[k] ?? k
}

/** 统计各类型的条数。键是类型取值，`''` 是未分类。 */
export function countRobotTypes<T>(
  rows: T[],
  keyOf: (r: T) => string | null | undefined,
): Record<string, number> {
  const out: Record<string, number> = {}
  for (const r of rows) {
    const k = (keyOf(r) ?? '').trim()
    out[k] = (out[k] ?? 0) + 1
  }
  return out
}

export interface RobotFilterOption {
  /** 'all' / 类型取值 / 'none'（见 ROBOT_FILTER_*）。 */
  value: string
  label: string
  count: number
}

/**
 * 生成筛选条选项。
 *
 * 计数一律基于**全量**数据（调用方要传未筛选的集合）：如果按筛选后的集合计数，
 * 一旦筛了「喷涂机器人」，其它选项立刻变成 0，管理员会误以为那些类型下面
 * 没有设备 —— 这是最容易被当成"功能没生效"的一种假象。
 *
 * 只输出数量大于 0 的类型：类型数量为 0 时留着选项，只会把筛选条撑满而
 * 提供不了任何信息。没有任何记录时只留「全部 0」。
 */
export function robotFilterOptions<T>(
  rows: T[],
  keyOf: (r: T) => string | null | undefined,
): RobotFilterOption[] {
  const counts = countRobotTypes(rows, keyOf)
  const opts: RobotFilterOption[] = [
    { value: ROBOT_FILTER_ALL, label: '全部', count: rows.length },
  ]
  for (const t of ROBOT_TYPES) {
    const n = counts[t] ?? 0
    if (n > 0) opts.push({ value: t, label: robotLabel(t), count: n })
  }
  const none = counts[''] ?? 0
  if (none > 0) opts.push({ value: ROBOT_FILTER_NONE, label: ROBOT_TYPE_NONE, count: none })
  return opts
}

/** 判断某条记录是否命中当前筛选。 */
export function matchesRobotFilter(
  v: string | null | undefined,
  filter: string,
): boolean {
  const k = (v ?? '').trim()
  if (filter === ROBOT_FILTER_ALL) return true
  if (filter === ROBOT_FILTER_NONE) return !k
  return k === filter
}

export interface RobotGroup<T> {
  /** 类型取值，空串表示未分类。 */
  key: RobotKey
  /** 中文标签（未分类时为「未分类」）。 */
  label: string
  items: T[]
}

export interface RobotArrangement<T> {
  /** 按分组顺序摊平后的列表 —— 表格直接拿它当 data。 */
  flat: T[]
  groups: RobotGroup<T>[]
  /**
   * 组首行 → 该组行数。表格的类型列靠它做纵向合并。
   *
   * 键用**行对象本身**而不是行号：列表一旦被筛选、刷新或重新排序，行号会
   * 整体位移，用它做键会让合并块错位到别的行上，而且不会报任何错。
   */
  span: Map<T, number>
}

/**
 * 按机器人类型分组并摊平，同时算出类型列的合并信息。
 *
 * 排序是分组的前提：合并单元格要求同类型的行彼此相邻，所以这里**必须**先排序
 * 再分组。也因此表格不能再让用户对类型列自定义排序（那会把合并块打散）。
 *
 * Array.prototype.sort 是稳定的，因此组内保持传入顺序 —— 调用方在外层
 * 做好的筛选与原有次序不会被这一步打乱。
 */
export function arrangeByRobotType<T>(
  rows: T[],
  keyOf: (r: T) => string | null | undefined,
): RobotArrangement<T> {
  const sorted = [...rows].sort((a, b) => robotTypeRank(keyOf(a)) - robotTypeRank(keyOf(b)))

  const flat: T[] = []
  const groups: RobotGroup<T>[] = []
  const span = new Map<T, number>()

  for (const r of sorted) {
    const k = (keyOf(r) ?? '').trim()
    let g = groups[groups.length - 1]
    if (!g || g.key !== k) {
      g = { key: k, label: robotLabel(k), items: [] }
      groups.push(g)
    }
    g.items.push(r)
    flat.push(r)
  }
  for (const g of groups) {
    span.set(g.items[0], g.items.length)
  }
  return { flat, groups, span }
}

/**
 * 生成 el-table 的 `span-method`：只对「机器人类型」这一列生效。
 *
 * typeColumnIndex 是该列在表格里的下标（从 0 起）。
 *
 * 已知取舍：被合并的单元格在 DOM 上属于**组首行**，所以点击合并块的中间部分
 * 仍会命中组首行的 row-click（客户端页据此打开详情）。类型列本身不是数据列，
 * 用户点它多半只是想看类型，因此接受这一处歧义；换来的是不引入树形表格、
 * 不改动其余任何一列与行内操作。
 */
export function robotSpanMethod<T>(span: Map<T, number>, typeColumnIndex: number) {
  return (data: { row: T; columnIndex: number }): { rowspan: number; colspan: number } | undefined => {
    if (data.columnIndex !== typeColumnIndex) return undefined
    const n = span.get(data.row)
    if (n === undefined) return { rowspan: 0, colspan: 0 }
    if (n <= 1) return undefined
    return { rowspan: n, colspan: 1 }
  }
}
