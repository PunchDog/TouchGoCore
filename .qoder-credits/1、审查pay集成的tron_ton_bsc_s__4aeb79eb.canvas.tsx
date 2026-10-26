import {
  BarChart,
  Callout,
  ChartComparisonGrid,
  ChartContainer,
  H1,
  LineChart,
  MetricsGrid,
  PieChart,
  ReportSection,
  ReportShell,
  Stack,
  Table,
  Text,
  type MetricItem,
} from "qoder/canvas";

// —— 报告数据契约（由 scripts/lib/breakdown.mjs 产出，plugin 注入）——
interface CatRow {
  key: string;
  label: string;
  tokens: number;
  share: number;
}
interface ToolRow {
  tool: string;
  tokens: number;
  calls: number;
  /** 平均每次调用带来的重发 token；无调用记录时 null（不给 0） */
  perCall?: number | null;
  share: number;
}
interface FileRow {
  attr: string;
  label: string;
  kind: string;
  tool: string | null;
  selfTokens: number;
  reads: number;
  trips: number;
  billed: number;
  /** 单次读取的总代价（含此后每一程重发）；reads=0 时 null */
  perRead?: number | null;
  share: number;
}
interface ReqRow {
  index: number;
  time: string | null;
  ratio: number;
  inputTokens: number;
  /** 代理或转录任一有真值即非 null；两者都没则 null（不写 0） */
  outputTokens?: number | null;
  /** proxy=代理实测 / transcript-tokens=转录 input_tokens / transcript-ratio=转录 ratio×window */
  usageSource?: "proxy" | "transcript-tokens" | "transcript-ratio" | "transcript";
  credits: number;
  originalCredits: number;
  afterCompact: boolean;
}
interface CatChild {
  tool: string | null;
  label: string;
  kind: string | null;
  tokens: number;
  share: number;
  catShare: number;
  trips: number;
}
interface CatDetail {
  key: string;
  label: string;
  tokens: number;
  share: number;
  children: CatChild[];
}
interface SubagentRow {
  agentId: string;
  agentType: string | null;
  description: string | null;
  toolUseId: string | null;
  roundTrips: number;
  billedInputTokens: number;
  peakContextRatio: number;
  credits: number;
  originalCredits: number;
  error: string | null;
}
interface SubagentTotals {
  roundTrips: number;
  billedInputTokens: number;
  credits: number;
  originalCredits: number;
}
interface Subagents {
  scanned: boolean;
  dir: string | null;
  /** 试过的候选目录；用于区分「真没子代理」与「路径没找对」。 */
  probedPaths?: string[];
  /** ok=已汇总 / no-dir=无子代理目录 / dir-empty=目录在但无 agent 文件 / no-usage=有文件但转录无 usage / error=读取抛错 */
  reason?: string;
  count: number;
  items: SubagentRow[];
  totals: SubagentTotals;
  combined: SubagentTotals;
}
/** 逐项可用性：报告里每个数字到底是模型上报的真值、按真值推导、回退默认、手工录入，还是本地根本没有。
 *  v2 旧报告没这个对象，故全部字段可选，读取时一律走默认值（旧报告只可能来自桌面端富转录）。 */
type Avail = "measured" | "derived" | "fallback" | "manual" | "unavailable";
interface Availability {
  credits?: Avail;
  roundTrips?: Avail;
  contextRatio?: Avail;
  tokens?: Avail;
  /** 输出 token：代理或转录 output_tokens 任一有真值即 measured */
  outputTokens?: Avail;
  /** 缓存命中 token：代理或转录 cache_read_input_tokens 任一有真值即 measured */
  cachedTokens?: Avail;
  categoryShare?: Avail;
  toolShare?: Avail;
  fileShare?: Avail;
  systemPrompt?: Avail;
  contextWindow?: Avail;
  compactions?: Avail;
  compactionCost?: Avail;
  model?: Avail;
  title?: Avail;
  toolCalls?: Avail;
  fileReads?: Avail;
  userTurns?: Avail;
  /** 用户压缩阈值：manual 覆盖 → manual；默认 200K → fallback */
  userContextLimit?: Avail;
}
/** 官方 UI 真值（手工录入，来自 .qoder-credits/overrides/<sessionId>.json）。
 *  IDE 端转录不含 usage，本地算不出 Credits，这是唯一的真值通道；绝不覆盖 totals，只并列展示。 */
interface ManualTruth {
  file?: string;
  credits: number | null;
  originalCredits: number | null;
  model: string | null;
  startedAt: string | null;
  endedAt: string | null;
  durationMin: number | null;
  note: string | null;
  /** 手工锁定模型窗口（如 qwen3-max=1000000），优先于 runtime-config / 反推 / fallback */
  contextWindow?: number | null;
  /** 手工指定 Qoder 压缩触发阈值（默认 200000），驱动 peakUserAdvice */
  userContextLimit?: number | null;
  /** 本地 credits ÷ 官方真值。有子代理时拿 combined 比（官方 UI 扣费 = 主链 + 子代理），
   *  否则覆盖率会被系统性低估（实测 ca2f7834：主链比 0.46、combined 比 0.89）。 */
  localCoverage?: number | null;
  /** main=只比主链 / combined=比主链+子代理 */
  localScope?: "main" | "combined";
  /** 参与对账的本地 credits（按 localScope 取 totals.credits 或 subagents.combined.credits） */
  localCredits?: number | null;
}
/** 峰值占比的「该不该压」结论。阈值算在数据层（breakdown.mjs peakAdvice），
 *  这里只管展示——否则 Canvas 与终端各持一套阈值迟早走偏。tone 直接喂 Callout。 */
interface PeakAdvice {
  level: "low" | "mid" | "sweet" | "high" | "over";
  tone: "info" | "success" | "warning" | "danger";
  text: string;
}
/** 一次压缩 = 一笔普通模型调用（整份上下文当 prompt、摘要当 completion），是会话里单笔最贵的开销。
 *  pre/post 是客户端自估口径；proxy 是能在代理日志里唯一对上时回填的供应商实测值，对不上就 null。 */
interface CompactionEvent {
  index: number;
  at: string | null;
  trigger: string | null;
  preTokens: number | null;
  postTokens: number | null;
  messagesSummarized: number | null;
  /** 压缩后首轮的往返序号与实测输入 = 压缩把上下文压到的地板 */
  nextRequestIndex: number | null;
  nextInputTokens: number | null;
  savedTokens: number | null;
  /** 数据层归一后的生效值：有代理实测就用实测，否则是客户端自估。
   *  渲染端直接取这两个，别自己按 proxy 分支——否则会与合计的口径分叉。 */
  effectiveInputTokens?: number | null;
  effectiveOutputTokens?: number | null;
  proxy?: {
    promptTokens: number;
    completionTokens: number;
    cachedTokens: number | null;
    ms: number | null;
    matchedBy: string;
  } | null;
}
interface CompactionCost {
  count: number;
  items: CompactionEvent[];
  /** measured = items 里有几笔拿到了供应商实测（其余为客户端自估） */
  totals: { preTokens: number; postTokens: number; savedTokens: number; measured?: number };
}
/** 链路健康度：代理覆盖率 + 最近记录时间 + 最近若干笔的三方归因计数。
 *  逐笔明细属于排障（CLI --request-log），报告只给一行结论。 */
interface LinkHealth {
  matched: number;
  requests: number;
  coverage: number;
  /** proxy / transcript-tokens / transcript-ratio / mixed / transcript（v3 旧报告）/ null */
  usageSource?: "proxy" | "transcript-tokens" | "transcript-ratio" | "mixed" | "transcript" | null;
  /** 三档拆分：代理实测 / 转录 input_tokens / 转录 ratio×window */
  breakdown?: { proxy: number; transcriptTokens: number; transcriptRatio: number };
  logRecords?: number;
  lastRecordAt?: string | null;
  /** 参与归因的最近笔数；0 = 运行中的代理是旧版或还没记到诊断 */
  recentRequests?: number;
  proxyErrors?: number | null;
  upstreamRejected?: number | null;
  aborted?: number | null;
  truncated?: number | null;
  noUsage?: number | null;
  slowestMs?: number | null;
  /** 被更新流量推翻的那条陈旧拉起失败记录；null = 没有 */
  supersededFailure?: { error: string; port?: number; at?: string | null } | null;
}
interface Report {
  schemaVersion: number;
  generatedAt: string;
  pluginVersion?: string;
  /** 转录来自哪一代客户端：桌面端富转录有 usage 真值，IDE 端精简转录没有。v2 旧报告无此字段。 */
  source?: "desktop-rich" | "ide-lite" | "unknown";
  /** usage 数字的来源：proxy=代理实测 / transcript-tokens=转录 input_tokens 真值 /
   *  transcript-ratio=转录 ratio×window 推导 / mixed=多档混合 / transcript=v3 旧报告兼容。无 usage 时 null。 */
  usageSource?: "proxy" | "transcript-tokens" | "transcript-ratio" | "mixed" | "transcript" | null;
  /** 可直接粘贴执行的插件入口命令（generate.mjs 用 proxy.mjs 的 selfCmd() 注入）。
   *  Windows 上是插件自带启动器的绝对路径，不要求用户装 Node；模板是静态文本，拿不到就只能硬编码。 */
  selfCmd?: string | null;
  /** 链路健康度一行结论（逐笔明细留给 CLI --request-log） */
  linkHealth?: LinkHealth | null;
  /** 代理 join 诊断（lib/proxylog.mjs）+ 配置/状态（generate.mjs 注入） */
  proxy?: {
    matched: number;
    requests: number;
    /** 三档拆分：代理实测 / 转录 input_tokens / 转录 ratio×window */
    breakdown?: { proxy: number; transcriptTokens: number; transcriptRatio: number };
    logPath?: string;
    logExists?: boolean;
    logRecords?: number;
    /** config.json 里 upstream 合法 = 用户已配置代理 */
    configured?: boolean;
    enabled?: boolean;
    port?: number;
    /** 最近一条代理记录的时间；null = 从无流量 */
    lastRecordAt?: string | null;
    /** 已配置但长期零流量（多半已改回官方模型）——软提示可 --stop-proxy */
    dormant?: boolean;
    /** 最近一次自动拉起失败（如端口被占）；ok 时为 null */
    status?: { error: string; port?: number; at?: string | null } | null;
    /** status 那条失败记录是否已被更新的流量推翻（代理在它之后还记到了流量）。
     *  status.json 只写不清，陈旧失败会把用户推去改本来正确的 Base URL，故必须判掉。 */
    statusSuperseded?: boolean;
  };
  availability?: Availability;
  manual?: ManualTruth | null;
  session: {
    id: string | null;
    title: string | null;
    /** custom-title / first-user / session-id —— 令产物文件名可解释 */
    titleSource?: string;
    model: string | null;
    /** runtime-config / manual / unavailable */
    modelSource?: string;
    cwd: string | null;
    turns: number;
    roundTrips: number;
    compactions: number;
    startedAt: string | null;
    endedAt: string | null;
  };
  context: {
    contextWindow: number;
    /** runtime-config=实测 / derived-from-usage=从 usage 反推 / manual=ManualTruth 手工锁定 /
     *  fallback=读不到静默回退 200000 / caller=调用方传入（旧枚举，兼容）。
     *  报告里每个 token 数字都要乘它，回退时必须标出来。 */
    contextWindowSource?: string;
    systemPromptTokens: number;
    netContextTokens: number;
    /** 峰值 = 当前窗口口径：自最近一次压缩起算，压缩边界处归零重新累积 */
    peakContextTokens: number;
    peakContextRatio: number;
    /** 本场历史峰值（跨压缩）；整场没压缩过时与上面相等 */
    peakSessionTokens?: number;
    peakSessionRatio?: number;
    /** 历史峰值是否值得单独交代（与当前窗口峰值相差 ≥1 个百分点），渲染端直接取用不再自判 */
    peakSessionNotable?: boolean;
    /** 峰值占比的「该不该压」结论（对模型窗口）；无有效占比时 null */
    peakAdvice?: PeakAdvice | null;
    /** 用户压缩阈值（Qoder 自动触发点），与模型窗口独立。默认 200000，ManualTruth 可覆盖 */
    userContextLimit?: number;
    /** default=内置 200K / manual=overrides 手写 / config=插件配置 / derived=实测反推 */
    userContextLimitSource?: "default" | "manual" | "config" | "derived";
    /** peakContextTokens ÷ userContextLimit */
    peakUserRatio?: number;
    /** 对用户阈值的「快自动压缩了吗」结论；与 peakAdvice 可矛盾（模型窗口未满但用户阈值已超） */
    peakUserAdvice?: PeakAdvice | null;
    /** v3.2 当前占用头条口径：段内单调递增 ⇒ 峰值≡当前，故以 netContextTokens 作头条，peak 退灰字 */
    currentContextTokens?: number;
    netUserRatio?: number;
    netUserAdvice?: PeakAdvice | null;
    netWindowRatio?: number;
    /** v3.2 自动压缩自校准：本场 trigger=auto 的实际触发点（无需外部配置）与「阈值是否被强制」判定 */
    observedAutoCompactions?: number;
    observedAutoTriggerTokens?: number | null;
    autoTriggerRatio?: number | null;
    thresholdNotEnforced?: boolean;
  };
  totals: {
    billedInputTokens: number;
    netContextTokens: number;
    peakContextTokens: number;
    amplification: number;
    attributedTokens: number;
    coverage: number;
    credits: number;
    originalCredits: number;
    /** 输出 token 总量：仅代理路径有真值，否则 0 且 availability.outputTokens=unavailable */
    outputTokens?: number;
    /** 供应商上下文缓存命中的那部分 prompt：仅代理路径有真值 */
    cachedTokens?: number;
    cachedTrips?: number;
    /** 缓存命中 ÷ 计费输入总量（全局实测约 91.5%：重复叠加的前缀正是缓存的命中对象） */
    cachedShare?: number;
    /** 两个不依赖 usage 的计数，IDE 端占比全缺时仍有可展示的真值 */
    toolCalls?: number;
    fileReads?: number;
  };
  /** credits 的分子到底覆盖了多少：与计费输入总量并排展示时，部分覆盖不说就等于把局部真值当全量。 */
  creditsCoverage?: {
    /** 真正累加进 totals.credits 的往返数 */
    trips: number;
    roundTrips: number;
    /** 这些往返的计费输入之和 */
    tokens: number;
    tokenShare: number;
    /** trips === roundTrips：全覆盖时不必再啰嗦覆盖范围 */
    full: boolean;
  };
  /** 压缩单笔成本（此前只显示「压缩 N 次」，而它是会话里单笔最贵的调用） */
  compactionCost?: CompactionCost;
  byCategory: CatRow[];
  byCategoryDetail: CatDetail[];
  byTool: ToolRow[];
  byFile: FileRow[];
  byRequest: ReqRow[];
  subagents?: Subagents;
  identity: { sumAttributed: number; billedInputTokens: number; absDiff: number; ok: boolean | null };
}

// 注入点：下一行的 REPORT 初值会被 render-canvas.mjs 按整行替换为真实报告 JSON。
const REPORT = {"schemaVersion":3.2,"generatedAt":"2026-10-26T01:36:47.615Z","source":"desktop-rich","usageSource":"transcript-ratio","availability":{"credits":"measured","roundTrips":"measured","contextRatio":"measured","tokens":"derived","outputTokens":"unavailable","cachedTokens":"unavailable","categoryShare":"derived","toolShare":"derived","fileShare":"derived","systemPrompt":"derived","contextWindow":"fallback","compactions":"measured","compactionCost":"measured","model":"measured","title":"measured","toolCalls":"measured","fileReads":"measured","userTurns":"measured","userContextLimit":"fallback"},"session":{"id":"4aeb79eb-5a0f-43ff-a744-4df540cc955d","title":"1、审查pay集成的tron/ton/bsc/s","titleSource":"first-user","model":"dfmodel","modelSource":"runtime-config","cwd":"D:\\TouchGoCore","turns":10,"roundTrips":332,"compactions":0,"startedAt":"2026-10-25T14:03:49.422Z","endedAt":"2026-10-26T01:36:46.786Z"},"context":{"contextWindow":200000,"contextWindowSource":"fallback","systemPromptTokens":0,"netContextTokens":111483,"peakContextTokens":111483,"peakContextRatio":0.557416,"peakSessionTokens":111483,"peakSessionRatio":0.557416,"peakSessionNotable":false,"peakAdvice":{"level":"mid","tone":"info","text":"已过盈亏线（39%）但还没进性价比区间，压缩有净收益但不大；不急着压"},"userContextLimit":200000,"userContextLimitSource":"default","peakUserRatio":0.557416,"peakUserAdvice":{"level":"mid","tone":"info","text":"已过盈亏线（39%）但还没进性价比区间，压缩有净收益但不大；不急着压"},"currentContextTokens":111483,"netUserRatio":0.557415,"netUserAdvice":{"level":"mid","tone":"info","text":"已过盈亏线（39%）但还没进性价比区间，压缩有净收益但不大；不急着压"},"netWindowRatio":0.557415,"observedAutoCompactions":0,"observedAutoTriggerTokens":null,"autoTriggerRatio":null,"thresholdNotEnforced":false},"totals":{"billedInputTokens":19853789,"netContextTokens":111483,"peakContextTokens":111483,"amplification":178.09,"attributedTokens":19853789,"coverage":1,"credits":657.151,"originalCredits":1506.157,"outputTokens":0,"cachedTokens":0,"cachedTrips":0,"cachedShare":0,"toolCalls":362,"fileReads":185},"creditsCoverage":{"trips":332,"roundTrips":332,"tokens":19853789,"tokenShare":1.0000000201472876,"full":true},"compactionCost":{"count":0,"items":[],"totals":{"preTokens":0,"postTokens":0,"savedTokens":0,"measured":0}},"proxy":{"matched":0,"requests":332,"breakdown":{"proxy":0,"transcriptTokens":0,"transcriptRatio":332},"logPath":"C:\\Users\\TouchGo\\.qoder-credits-proxy\\usage.jsonl","logExists":false,"logRecords":0,"configured":false,"enabled":true,"port":49787,"lastRecordAt":null,"dormant":false,"status":null,"statusSuperseded":false},"byCategory":[{"key":"tool_result","label":"工具返回","tokens":6644977,"share":0.3346956442920015},{"key":"assistant_thinking","label":"模型思考","tokens":6083376,"share":0.3064088042822216},{"key":"assistant_tool_use","label":"工具调用","tokens":2175460,"share":0.10957407168781375},{"key":"assistant_text","label":"模型回复","tokens":612646,"share":0.030857910078092632},{"key":"user_input","label":"用户输入","tokens":10525,"share":0.0005301176229342103},{"key":"attachment","label":"附件/技能","tokens":4326805,"share":0.21793347218423284}],"byCategoryDetail":[{"key":"tool_result","label":"工具返回","tokens":6644977,"share":0.3346956442920015,"children":[{"tool":"Bash","label":"（无路径）","kind":"shell","tokens":2765517,"share":0.1392941825633025,"catShare":0.4161816412578612,"trips":22223},{"tool":"Read","label":"bsc/onchain.go","kind":"read","tokens":865674,"share":0.043602446893279204,"catShare":0.13027491584335868,"trips":1079},{"tool":"Read","label":"ton/query.go","kind":"read","tokens":269606,"share":0.013579550557105757,"catShare":0.040572833225336,"trips":315},{"tool":"Agent","label":"（无路径）","kind":"other","tokens":212617,"share":0.01070913395226344,"catShare":0.03199663376234552,"trips":1098},{"tool":"Read","label":"tron/query.go","kind":"read","tokens":203661,"share":0.010258024077245626,"catShare":0.030648812591944363,"trips":317},{"tool":"Read","label":"ledger/ledger.go","kind":"read","tokens":184167,"share":0.00927616501228839,"catShare":0.027715224773572206,"trips":317},{"tool":"Read","label":"localtimer/wheel_slots.go","kind":"read","tokens":160560,"share":0.008087115202161028,"catShare":0.024162594703818475,"trips":107},{"tool":"Read","label":"tron/route.go","kind":"read","tokens":157172,"share":0.007916478735299264,"catShare":0.023652768926961654,"trips":253},{"tool":"Read","label":"bsc/route.go","kind":"read","tokens":125856,"share":0.006339154155457344,"catShare":0.018940055729935998,"trips":245},{"tool":"Read","label":"sol/onchain.go","kind":"read","tokens":117026,"share":0.005894411635093944,"catShare":0.017611258872408362,"trips":1085},{"tool":"Read","label":"bsc/route_test.go","kind":"read","tokens":116728,"share":0.005879391041722198,"catShare":0.01756638050715948,"trips":239},{"tool":"Read","label":"ton/route.go","kind":"read","tokens":114351,"share":0.0057596466498287075,"catShare":0.017208609517498735,"trips":245},{"tool":"Read","label":"pay/transfer.go","kind":"read","tokens":111456,"share":0.005613836688846105,"catShare":0.016772960104459486,"trips":242},{"tool":"Read","label":"util/string.go","kind":"read","tokens":79759,"share":0.00401731893655876,"catShare":0.01200290175588271,"trips":339},{"tool":"Read","label":"websocket/client.go","kind":"read","tokens":79027,"share":0.00398045131755092,"catShare":0.011892749085429444,"trips":599},{"tool":null,"label":"其他 80 项","kind":null,"tokens":1081800,"share":0.054488336873991856,"catShare":0.1627996593420084,"trips":33929}]},{"key":"assistant_thinking","label":"模型思考","tokens":6083376,"share":0.3064088042822216,"children":[]},{"key":"assistant_tool_use","label":"工具调用","tokens":2175460,"share":0.10957407168781375,"children":[{"tool":"Bash","label":"（无路径）","kind":"shell","tokens":369612,"share":0.01861672137825975,"catShare":0.1699007903192686,"trips":22223},{"tool":"Write","label":"chainroute/chainroute.go","kind":"write","tokens":179843,"share":0.009058384220796713,"catShare":0.08266904826357875,"trips":235},{"tool":"Agent","label":"（无路径）","kind":"other","tokens":138165,"share":0.006959113166721029,"catShare":0.06351058292830589,"trips":1098},{"tool":"Edit","label":"bsc/onchain.go","kind":"write","tokens":134769,"share":0.0067880628296457185,"catShare":0.06194953536987758,"trips":1198},{"tool":"Write","label":"chainroute/route_test.go","kind":"write","tokens":123556,"share":0.006223276124856033,"catShare":0.056795152621385646,"trips":233},{"tool":"Edit","label":"tron/usdt.go","kind":"write","tokens":97520,"share":0.0049118957535934345,"catShare":0.044827171957138374,"trips":832},{"tool":"Edit","label":"ton/native.go","kind":"write","tokens":87102,"share":0.0043871489476654075,"catShare":0.04003820319979332,"trips":935},{"tool":"Edit","label":"sol/usdt.go","kind":"write","tokens":77702,"share":0.0039137120015389935,"catShare":0.035717500876388954,"trips":882},{"tool":"Edit","label":"bsc/usdt.go","kind":"write","tokens":75254,"share":0.003790416748791377,"catShare":0.034592277994292396,"trips":910},{"tool":"Edit","label":"sol/onchain.go","kind":"write","tokens":69615,"share":0.0035063990366619876,"catShare":0.03200026231252983,"trips":1219},{"tool":"Edit","label":"tron/query.go","kind":"write","tokens":55396,"share":0.0027901768337037476,"catShare":0.025463841862637074,"trips":554},{"tool":"TaskCreate","label":"（无路径）","kind":"other","tokens":49200,"share":0.002478098682892601,"catShare":0.022615739697552945,"trips":2915},{"tool":"Write","label":"bsc/route_test.go","kind":"write","tokens":46611,"share":0.0023477086134725744,"catShare":0.02142576776886967,"trips":225},{"tool":"Edit","label":"bsc/per_order_test.go","kind":"write","tokens":46531,"share":0.002343672499838042,"catShare":0.02138893320050543,"trips":583},{"tool":"Edit","label":"util/string.go","kind":"write","tokens":44726,"share":0.0022527924826308806,"catShare":0.020559539751788054,"trips":673},{"tool":null,"label":"其他 80 项","kind":null,"tokens":579860,"share":0.029206492366743293,"catShare":0.2665456518760677,"trips":27917}]},{"key":"assistant_text","label":"模型回复","tokens":612646,"share":0.030857910078092632,"children":[]},{"key":"user_input","label":"用户输入","tokens":10525,"share":0.0005301176229342103,"children":[]},{"key":"attachment","label":"附件/技能","tokens":4326805,"share":0.21793347218423284,"children":[]}],"byTool":[{"tool":"Read","tokens":3480721,"calls":68,"perCall":51187,"share":0.17531771668593604},{"tool":"Bash","tokens":3135130,"calls":146,"perCall":21473,"share":0.1579109039415636},{"tool":"Edit","tokens":1094801,"calls":97,"perCall":11287,"share":0.05514318083666479},{"tool":"Write","tokens":521262,"calls":11,"perCall":47387,"share":0.026255016167921287},{"tool":"Agent","tokens":350782,"calls":4,"perCall":87695,"share":0.01766824711898446},{"tool":"Grep","tokens":137764,"calls":9,"perCall":15307,"share":0.006938908451334171},{"tool":"TaskCreate","tokens":63922,"calls":10,"perCall":6392,"share":0.0032196250669878227},{"tool":"AskUserQuestion","tokens":24129,"calls":1,"perCall":24129,"share":0.0012153325554976897},{"tool":"TaskUpdate","tokens":11928,"calls":16,"perCall":745,"share":0.0006007851549179993}],"byFile":[{"attr":"file:D:\\TouchGoCore\\bsc\\onchain.go","label":"bsc/onchain.go","kind":"read","tool":"Read","selfTokens":15312,"reads":9,"trips":4554,"billed":1011975,"perRead":112442,"share":0.05097135530875884},{"attr":"file:D:\\TouchGoCore\\ton\\query.go","label":"ton/query.go","kind":"read","tool":"Read","selfTokens":4733,"reads":2,"trips":1194,"billed":313338,"perRead":156669,"share":0.015782263765064003},{"attr":"file:D:\\TouchGoCore\\tron\\query.go","label":"tron/query.go","kind":"read","tool":"Read","selfTokens":4024,"reads":3,"trips":1742,"billed":264023,"perRead":88008,"share":0.013298347044662992},{"attr":"file:D:\\TouchGoCore\\sol\\onchain.go","label":"sol/onchain.go","kind":"read","tool":"Read","selfTokens":3206,"reads":9,"trips":4608,"billed":195539,"perRead":21727,"share":0.009848966699655516},{"attr":"file:D:\\TouchGoCore\\tron\\route.go","label":"tron/route.go","kind":"read","tool":"Read","selfTokens":3672,"reads":2,"trips":928,"billed":185187,"perRead":92594,"share":0.009327551529255244},{"attr":"file:D:\\TouchGoCore\\ledger\\ledger.go","label":"ledger/ledger.go","kind":"read","tool":"Read","selfTokens":2715,"reads":1,"trips":634,"billed":185053,"perRead":185053,"share":0.009320794969786442},{"attr":"file:D:\\TouchGoCore\\chainroute\\chainroute.go","label":"chainroute/chainroute.go","kind":"write","tool":"Write","selfTokens":3777,"reads":1,"trips":470,"billed":181477,"perRead":181477,"share":0.009140667165896118},{"attr":"file:D:\\TouchGoCore\\bsc\\route_test.go","label":"bsc/route_test.go","kind":"read","tool":"Read","selfTokens":3450,"reads":2,"trips":928,"billed":165581,"perRead":82790,"share":0.008340009007976972},{"attr":"file:D:\\TouchGoCore\\localtimer\\wheel_slots.go","label":"localtimer/wheel_slots.go","kind":"read","tool":"Read","selfTokens":7587,"reads":1,"trips":214,"billed":160878,"perRead":160878,"share":0.008103135636396688},{"attr":"file:D:\\TouchGoCore\\bsc\\route.go","label":"bsc/route.go","kind":"read","tool":"Read","selfTokens":3031,"reads":2,"trips":950,"billed":150597,"perRead":75298,"share":0.007585298620820612},{"attr":"file:D:\\TouchGoCore\\tron\\usdt.go","label":"tron/usdt.go","kind":"write","tool":"Edit","selfTokens":3333,"reads":6,"trips":2490,"billed":139907,"perRead":23318,"share":0.007046853881922401},{"attr":"file:D:\\TouchGoCore\\ton\\route.go","label":"ton/route.go","kind":"read","tool":"Read","selfTokens":2857,"reads":2,"trips":870,"billed":136806,"perRead":68403,"share":0.006890684184913659},{"attr":"file:D:\\TouchGoCore\\util\\string.go","label":"util/string.go","kind":"read","tool":"Read","selfTokens":3803,"reads":6,"trips":2024,"billed":130284,"perRead":21714,"share":0.006562183245756511},{"attr":"file:D:\\TouchGoCore\\chainroute\\route_test.go","label":"chainroute/route_test.go","kind":"write","tool":"Write","selfTokens":2629,"reads":1,"trips":466,"billed":125174,"perRead":125174,"share":0.006304814232079582},{"attr":"file:D:\\TouchGoCore\\bsc\\per_order_test.go","label":"bsc/per_order_test.go","kind":"read","tool":"Read","selfTokens":1978,"reads":4,"trips":2344,"billed":122272,"perRead":30568,"share":0.006158616387900415},{"attr":"file:D:\\TouchGoCore\\pay\\transfer.go","label":"pay/transfer.go","kind":"read","tool":"Read","selfTokens":2260,"reads":1,"trips":484,"billed":112051,"perRead":112051,"share":0.005643803788608627},{"attr":"file:D:\\TouchGoCore\\tron\\double_pay_test.go","label":"tron/double_pay_test.go","kind":"read","tool":"Read","selfTokens":1920,"reads":3,"trips":1658,"billed":110239,"perRead":36746,"share":0.005552534125698084},{"attr":"file:D:\\TouchGoCore\\websocket\\client.go","label":"websocket/client.go","kind":"read","tool":"Read","selfTokens":9894,"reads":28,"trips":2252,"billed":100264,"perRead":3581,"share":0.005050125294033224},{"attr":"file:D:\\TouchGoCore\\ton\\native.go","label":"ton/native.go","kind":"write","tool":"Edit","selfTokens":2409,"reads":5,"trips":1870,"billed":90690,"perRead":18138,"share":0.004567888097517695},{"attr":"file:D:\\TouchGoCore\\list\\list.go","label":"list/list.go","kind":"read","tool":"Read","selfTokens":4305,"reads":8,"trips":1516,"billed":89921,"perRead":11240,"share":0.004529184352346852},{"attr":"file:D:\\TouchGoCore\\tron\\onchain.go","label":"tron/onchain.go","kind":"read","tool":"Read","selfTokens":2286,"reads":7,"trips":2420,"billed":85426,"perRead":12204,"share":0.00430273271822071},{"attr":"file:D:\\TouchGoCore\\sol\\usdt.go","label":"sol/usdt.go","kind":"write","tool":"Edit","selfTokens":1812,"reads":4,"trips":1764,"billed":80935,"perRead":20234,"share":0.004076563853502136},{"attr":"file:D:\\TouchGoCore\\bsc\\usdt.go","label":"bsc/usdt.go","kind":"write","tool":"Edit","selfTokens":1702,"reads":4,"trips":1820,"billed":78596,"perRead":19649,"share":0.003958752604469773},{"attr":"file:D:\\TouchGoCore\\sol\\onchain_test.go","label":"sol/onchain_test.go","kind":"read","tool":"Read","selfTokens":1203,"reads":2,"trips":1228,"billed":78511,"perRead":39256,"share":0.003954471338305289},{"attr":"file:D:\\TouchGoCore\\pay\\types.go","label":"pay/types.go","kind":"read","tool":"Read","selfTokens":975,"reads":1,"trips":656,"billed":70417,"perRead":70417,"share":0.003546766605601311},{"attr":"file:D:\\TouchGoCore\\ton\\onchain.go","label":"ton/onchain.go","kind":"read","tool":"Read","selfTokens":1526,"reads":8,"trips":3110,"billed":67954,"perRead":8494,"share":0.003422700778119865},{"attr":"file:D:\\TouchGoCore\\ton\\route_test.go","label":"ton/route_test.go","kind":"read","tool":"Read","selfTokens":1455,"reads":2,"trips":838,"billed":67718,"perRead":33859,"share":0.003410823510704314},{"attr":"file:D:\\TouchGoCore\\bsc\\query.go","label":"bsc/query.go","kind":"read","tool":"Read","selfTokens":924,"reads":2,"trips":1186,"billed":57949,"perRead":28974,"share":0.0029187811342217544},{"attr":"file:D:\\TouchGoCore\\list\\node.go","label":"list/node.go","kind":"read","tool":"Read","selfTokens":2845,"reads":6,"trips":1170,"billed":57742,"perRead":9624,"share":0.002908350081583248},{"attr":"file:D:\\TouchGoCore\\sol\\query.go","label":"sol/query.go","kind":"read","tool":"Read","selfTokens":876,"reads":2,"trips":1184,"billed":54639,"perRead":27320,"share":0.002752087624047768},{"attr":"file:D:\\TouchGoCore\\pay\\channel.go","label":"pay/channel.go","kind":"read","tool":"Read","selfTokens":1080,"reads":1,"trips":482,"billed":53309,"perRead":53309,"share":0.0026850819254385498},{"attr":"file:D:\\TouchGoCore\\tron\\usdt_test.go","label":"tron/usdt_test.go","kind":"write","tool":"Edit","selfTokens":1218,"reads":4,"trips":1598,"billed":49545,"perRead":12386,"share":0.0024955041145645067},{"attr":"file:D:\\TouchGoCore\\sol\\route_test.go","label":"sol/route_test.go","kind":"write","tool":"Write","selfTokens":951,"reads":1,"trips":436,"billed":42194,"perRead":42194,"share":0.0021252264467944465},{"attr":"file:computeUnitLimit|signatureFeeLamports|PriorityFeeMicroLamports|MicroLamports|ComputeUnitPrice|microLamports|1e6|1_000_000","label":"computeUnitLimit|signatureFeeLamports|PriorityFeeMicroLamports|MicroLamports|ComputeUnitPrice|microLamports|1e6|1_000_000","kind":"search","tool":"Grep","selfTokens":523,"reads":1,"trips":622,"billed":34746,"perRead":34746,"share":0.0017500822145682868},{"attr":"file:\\.nonces|adoptNonce|allocateNonce|nonceMu","label":"/.nonces|adoptNonce|allocateNonce|nonceMu","kind":"search","tool":"Grep","selfTokens":535,"reads":1,"trips":598,"billed":33784,"perRead":33784,"share":0.0017016325660346397},{"attr":"file:D:\\TouchGoCore\\bsc\\onchain_test.go","label":"bsc/onchain_test.go","kind":"read","tool":"Read","selfTokens":493,"reads":1,"trips":588,"billed":30472,"perRead":30472,"share":0.0015348455027564569},{"attr":"file:WithLock|func .*QueryOrder","label":"WithLock|func .*QueryOrder","kind":"search","tool":"Grep","selfTokens":422,"reads":1,"trips":628,"billed":28389,"perRead":28389,"share":0.0014298833297849973},{"attr":"file:D:\\TouchGoCore\\websocket\\client_pool_stats_test.go","label":"websocket/client_pool_stats_test.go","kind":"read","tool":"Read","selfTokens":2324,"reads":2,"trips":232,"billed":26981,"perRead":13491,"share":0.0013589894188306406},{"attr":"file:C:\\Users\\TouchGo\\.qoder-cn\\projects\\D--TouchGoCore\\memory\\pay-race-red-baseline-2026-10-25.md","label":"C:/Users/TouchGo/.qoder-cn/projects/D--TouchGoCore/memory/pay-race-red-baseline-2026-10-25.md","kind":"read","tool":"Read","selfTokens":3724,"reads":5,"trips":460,"billed":23536,"perRead":4707,"share":0.001185481908141321},{"attr":"file:D:\\TouchGoCore\\sol\\route.go","label":"sol/route.go","kind":"write","tool":"Write","selfTokens":513,"reads":1,"trips":446,"billed":23313,"perRead":23313,"share":0.0011742236267125235},{"attr":"file:D:\\TouchGoCore\\ton\\wallet_lock_test.go","label":"ton/wallet_lock_test.go","kind":"read","tool":"Read","selfTokens":344,"reads":1,"trips":626,"billed":23044,"perRead":23044,"share":0.0011607101769931056},{"attr":"file:D:\\TouchGoCore\\list\\qa_gl_range_pool_test.go","label":"list/qa_gl_range_pool_test.go","kind":"read","tool":"Read","selfTokens":1089,"reads":1,"trips":212,"billed":22873,"perRead":22873,"share":0.0011520735162538114},{"attr":"file:D:\\TouchGoCore\\bsc\\rpc_test.go","label":"bsc/rpc_test.go","kind":"read","tool":"Read","selfTokens":749,"reads":4,"trips":1138,"billed":21429,"perRead":5357,"share":0.0010793181856970264},{"attr":"file:D:\\TouchGoCore\\list\\pool.go","label":"list/pool.go","kind":"read","tool":"Read","selfTokens":870,"reads":2,"trips":414,"billed":17607,"perRead":8804,"share":0.0008868528616800948},{"attr":"file:facebook|instagram|fb_cookie|ins\\b","label":"facebook|instagram|fb_cookie|ins/b","kind":"search","tool":"Grep","selfTokens":232,"reads":1,"trips":662,"billed":17063,"perRead":17063,"share":0.0008594344652979829},{"attr":"file:D:\\TouchGoCore\\websocket\\p0_close_protocol_test.go","label":"websocket/p0_close_protocol_test.go","kind":"read","tool":"Read","selfTokens":1512,"reads":4,"trips":376,"billed":16021,"perRead":4005,"share":0.0008069244744871534},{"attr":"file:C:\\Users\\TouchGo\\.qoder-cn\\memory\\feedback-stepwise-slice-delivery.md","label":"C:/Users/TouchGo/.qoder-cn/memory/feedback-stepwise-slice-delivery.md","kind":"read","tool":"Read","selfTokens":471,"reads":1,"trips":256,"billed":11993,"perRead":11993,"share":0.0006040799889401775},{"attr":"file:C:\\Users\\TouchGo\\.qoder-cn\\memory\\user-stoneage-migration-profile.md","label":"C:/Users/TouchGo/.qoder-cn/memory/user-stoneage-migration-profile.md","kind":"read","tool":"Read","selfTokens":411,"reads":1,"trips":256,"billed":10465,"perRead":10465,"share":0.0005271271241070337},{"attr":"file:func newTestChannel|PendingNonce|func.*qaBSCPerOrderKey","label":"func newTestChannel|PendingNonce|func.*qaBSCPerOrderKey","kind":"search","tool":"Grep","selfTokens":155,"reads":1,"trips":590,"billed":9622,"perRead":9622,"share":0.00048463001102751034},{"attr":"file:D:\\TouchGoCore\\tron\\usdt_assembly_test.go","label":"tron/usdt_assembly_test.go","kind":"write","tool":"Edit","selfTokens":208,"reads":2,"trips":814,"billed":8588,"perRead":4294,"share":0.000432568946301871},{"attr":"file:C:\\Users\\TouchGo\\.qoder-cn\\memory\\feedback-long-verification-background.md","label":"C:/Users/TouchGo/.qoder-cn/memory/feedback-long-verification-background.md","kind":"read","tool":"Read","selfTokens":326,"reads":1,"trips":256,"billed":8301,"perRead":8301,"share":0.00041811056559341414},{"attr":"file:D:\\TouchGoCore\\tron\\account.go","label":"tron/account.go","kind":"read","tool":"Read","selfTokens":257,"reads":3,"trips":954,"billed":8203,"perRead":2734,"share":0.00041317102689722814},{"attr":"file:D:\\TouchGoCore\\util\\string_test.go","label":"util/string_test.go","kind":"write","tool":"Write","selfTokens":284,"reads":1,"trips":288,"billed":8173,"perRead":8173,"share":0.0004116704205540361},{"attr":"file:D:\\TouchGoCore\\list\\p1_list_concurrency_test.go","label":"list/p1_list_concurrency_test.go","kind":"read","tool":"Read","selfTokens":383,"reads":1,"trips":188,"billed":7125,"perRead":7125,"share":0.0003588794328765632},{"attr":"file:func buildableTx|type txReceipt|func newTestChannel","label":"func buildableTx|type txReceipt|func newTestChannel","kind":"search","tool":"Grep","selfTokens":86,"reads":1,"trips":552,"billed":4934,"perRead":4934,"share":0.0002485376807391337},{"attr":"file:D:\\TouchGoCore\\ton\\assembly_test.go","label":"ton/assembly_test.go","kind":"write","tool":"Edit","selfTokens":119,"reads":1,"trips":368,"billed":4420,"perRead":4420,"share":0.00022261656049121832},{"attr":"file:D:\\TouchGoCore\\ton\\nilcfg_test.go","label":"ton/nilcfg_test.go","kind":"write","tool":"Edit","selfTokens":120,"reads":2,"trips":730,"billed":4419,"perRead":2209,"share":0.00022255945598900206},{"attr":"file:pay\\types.go","label":"pay/types.go","kind":"search","tool":"Grep","selfTokens":54,"reads":1,"trips":658,"billed":3924,"perRead":3924,"share":0.00019762477178474153},{"attr":"file:facebook|instagram|fbapi|messenger|graph\\.facebook","label":"facebook|instagram|fbapi|messenger|graph/.facebook","kind":"search","tool":"Grep","selfTokens":48,"reads":1,"trips":660,"billed":3509,"perRead":3509,"share":0.00017673499949011565},{"attr":"file:D:\\TouchGoCore\\tron\\gateway.go","label":"tron/gateway.go","kind":"write","tool":"Edit","selfTokens":160,"reads":1,"trips":162,"billed":2562,"perRead":2562,"share":0.00012904150857611372},{"attr":"file:PriorityFeeMicroLamports","label":"PriorityFeeMicroLamports","kind":"search","tool":"Grep","selfTokens":27,"reads":1,"trips":622,"billed":1794,"perRead":1794,"share":0.00009034841260677581},{"attr":"file:D:\\TouchGoCore\\websocket\\websocket.go","label":"websocket/websocket.go","kind":"read","tool":"Read","selfTokens":123,"reads":1,"trips":88,"billed":1067,"perRead":1067,"share":0.00005374891401919735}],"byAttr":[{"attr":"assistant_thinking","tokens":6083376,"share":0.3064088042822216},{"attr":"attachment","tokens":4326805,"share":0.21793347218423284},{"attr":"tool_result|Read","tokens":3437578,"share":0.17314469061890136},{"attr":"tool_result|Bash","tokens":2765517,"share":0.1392941825633025},{"attr":"tool_use|Edit","tokens":1024868,"share":0.05162075846111869},{"attr":"assistant_text","tokens":612646,"share":0.030857910078092632},{"attr":"tool_use|Write","tokens":505977,"share":0.025485166738174685},{"attr":"tool_use|Bash","tokens":369612,"share":0.01861672137825975},{"attr":"tool_result|Agent","tokens":212617,"share":0.01070913395226344},{"attr":"tool_use|Agent","tokens":138165,"share":0.006959113166721029},{"attr":"tool_result|Grep","tokens":119793,"share":0.006033769935955235},{"attr":"tool_result|Edit","tokens":69933,"share":0.0035224223755470825},{"attr":"tool_use|TaskCreate","tokens":49200,"share":0.002478098682892601},{"attr":"tool_use|Read","tokens":43143,"share":0.0021730260670339905},{"attr":"tool_use|AskUserQuestion","tokens":19191,"share":0.0009666252603852937},{"attr":"tool_use|Grep","tokens":17970,"share":0.0009051385153789379},{"attr":"tool_result|Write","tokens":15284,"share":0.0007698494297466032},{"attr":"tool_result|TaskCreate","tokens":14722,"share":0.0007415263840952211},{"attr":"user_input","tokens":10525,"share":0.0005301176229342103},{"attr":"tool_use|TaskUpdate","tokens":7334,"share":0.0003694234178468678},{"attr":"tool_result|AskUserQuestion","tokens":4938,"share":0.0002487072951123965},{"attr":"tool_result|TaskUpdate","tokens":4593,"share":0.00023136173707113847},{"attr":"system","tokens":0,"share":0}],"byRequest":[{"index":1,"time":"22:03","ratio":0.034884,"inputTokens":6977,"outputTokens":null,"usageSource":"transcript-ratio","credits":4.738,"originalCredits":11.845,"afterCompact":false},{"index":2,"time":"22:04","ratio":0.035602,"inputTokens":7120,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.52,"originalCredits":1.299,"afterCompact":false},{"index":3,"time":"22:04","ratio":0.036195,"inputTokens":7239,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.523,"originalCredits":1.308,"afterCompact":false},{"index":4,"time":"22:04","ratio":0.036796,"inputTokens":7359,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.528,"originalCredits":1.319,"afterCompact":false},{"index":5,"time":"22:04","ratio":0.038064,"inputTokens":7613,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.62,"originalCredits":1.55,"afterCompact":false},{"index":6,"time":"22:04","ratio":0.039324,"inputTokens":7865,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.664,"originalCredits":1.661,"afterCompact":false},{"index":7,"time":"22:05","ratio":0.039701,"inputTokens":7940,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.548,"originalCredits":1.37,"afterCompact":false},{"index":8,"time":"22:05","ratio":0.039977,"inputTokens":7995,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.704,"originalCredits":1.761,"afterCompact":false},{"index":9,"time":"22:06","ratio":0.040825,"inputTokens":8165,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.144,"originalCredits":2.86,"afterCompact":false},{"index":10,"time":"22:14","ratio":0.045618,"inputTokens":9124,"outputTokens":null,"usageSource":"transcript-ratio","credits":6.16,"originalCredits":15.399,"afterCompact":false},{"index":11,"time":"22:14","ratio":0.047003,"inputTokens":9401,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.786,"originalCredits":1.964,"afterCompact":false},{"index":12,"time":"22:15","ratio":0.047398,"inputTokens":9480,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.916,"originalCredits":2.289,"afterCompact":false},{"index":13,"time":"22:15","ratio":0.048548,"inputTokens":9710,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.678,"originalCredits":1.694,"afterCompact":false},{"index":14,"time":"22:15","ratio":0.049111,"inputTokens":9822,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.632,"originalCredits":1.58,"afterCompact":false},{"index":15,"time":"22:16","ratio":0.049321,"inputTokens":9864,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.581,"originalCredits":1.451,"afterCompact":false},{"index":16,"time":"22:16","ratio":0.055423,"inputTokens":11085,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.369,"originalCredits":3.422,"afterCompact":false},{"index":17,"time":"22:17","ratio":0.069372,"inputTokens":13874,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.274,"originalCredits":8.186,"afterCompact":false},{"index":18,"time":"22:17","ratio":0.077561,"inputTokens":15512,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.899,"originalCredits":4.748,"afterCompact":false},{"index":19,"time":"22:17","ratio":0.078292,"inputTokens":15658,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.013,"originalCredits":2.532,"afterCompact":false},{"index":20,"time":"22:17","ratio":0.079838,"inputTokens":15968,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.232,"originalCredits":3.08,"afterCompact":false},{"index":21,"time":"22:18","ratio":0.081386,"inputTokens":16277,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.222,"originalCredits":3.055,"afterCompact":false},{"index":22,"time":"22:18","ratio":0.08258,"inputTokens":16516,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.371,"originalCredits":3.428,"afterCompact":false},{"index":23,"time":"22:19","ratio":0.084703,"inputTokens":16941,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.68,"originalCredits":4.2,"afterCompact":false},{"index":24,"time":"22:20","ratio":0.086335,"inputTokens":17267,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.645,"originalCredits":4.113,"afterCompact":false},{"index":25,"time":"22:20","ratio":0.088179,"inputTokens":17636,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.714,"originalCredits":4.284,"afterCompact":false},{"index":26,"time":"22:21","ratio":0.089922,"inputTokens":17984,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.294,"originalCredits":3.236,"afterCompact":false},{"index":27,"time":"22:21","ratio":0.090335,"inputTokens":18067,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.281,"originalCredits":3.202,"afterCompact":false},{"index":28,"time":"22:21","ratio":0.091201,"inputTokens":18240,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.491,"originalCredits":3.727,"afterCompact":false},{"index":29,"time":"22:22","ratio":0.092523,"inputTokens":18505,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.206,"originalCredits":3.015,"afterCompact":false},{"index":30,"time":"22:22","ratio":0.092732,"inputTokens":18546,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.048,"originalCredits":2.62,"afterCompact":false},{"index":31,"time":"22:23","ratio":0.092865,"inputTokens":18573,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.992,"originalCredits":4.979,"afterCompact":false},{"index":32,"time":"22:23","ratio":0.095993,"inputTokens":19199,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.707,"originalCredits":4.266,"afterCompact":false},{"index":33,"time":"22:24","ratio":0.096978,"inputTokens":19396,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.199,"originalCredits":2.998,"afterCompact":false},{"index":34,"time":"22:24","ratio":0.097825,"inputTokens":19565,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.269,"originalCredits":3.174,"afterCompact":false},{"index":35,"time":"22:25","ratio":0.098238,"inputTokens":19648,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.174,"originalCredits":2.935,"afterCompact":false},{"index":36,"time":"22:25","ratio":0.098506,"inputTokens":19701,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.2,"originalCredits":3.001,"afterCompact":false},{"index":37,"time":"22:26","ratio":0.10072,"inputTokens":20144,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.492,"originalCredits":3.73,"afterCompact":false},{"index":38,"time":"22:26","ratio":0.101361,"inputTokens":20272,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.193,"originalCredits":2.983,"afterCompact":false},{"index":39,"time":"22:26","ratio":0.102115,"inputTokens":20423,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.489,"originalCredits":3.723,"afterCompact":false},{"index":40,"time":"22:26","ratio":0.1031,"inputTokens":20620,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.303,"originalCredits":3.257,"afterCompact":false},{"index":41,"time":"22:27","ratio":0.103393,"inputTokens":20679,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.665,"originalCredits":4.163,"afterCompact":false},{"index":42,"time":"22:27","ratio":0.105074,"inputTokens":21015,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.409,"originalCredits":3.522,"afterCompact":false},{"index":43,"time":"22:27","ratio":0.105361,"inputTokens":21072,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.329,"originalCredits":3.323,"afterCompact":false},{"index":44,"time":"22:27","ratio":0.105927,"inputTokens":21185,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.293,"originalCredits":3.233,"afterCompact":false},{"index":45,"time":"22:28","ratio":0.123904,"inputTokens":24781,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.588,"originalCredits":8.969,"afterCompact":false},{"index":46,"time":"22:28","ratio":0.124468,"inputTokens":24894,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.452,"originalCredits":3.629,"afterCompact":false},{"index":47,"time":"22:28","ratio":0.124697,"inputTokens":24939,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.406,"originalCredits":3.516,"afterCompact":false},{"index":48,"time":"22:28","ratio":0.125175,"inputTokens":25035,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.633,"originalCredits":4.082,"afterCompact":false},{"index":49,"time":"22:29","ratio":0.125951,"inputTokens":25190,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.653,"originalCredits":4.134,"afterCompact":false},{"index":50,"time":"22:29","ratio":0.126671,"inputTokens":25334,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.654,"originalCredits":4.136,"afterCompact":false},{"index":51,"time":"22:29","ratio":0.127391,"inputTokens":25478,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.617,"originalCredits":4.041,"afterCompact":false},{"index":52,"time":"22:29","ratio":0.127954,"inputTokens":25591,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.617,"originalCredits":4.043,"afterCompact":false},{"index":53,"time":"22:30","ratio":0.12856,"inputTokens":25712,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.539,"originalCredits":3.846,"afterCompact":false},{"index":54,"time":"22:30","ratio":0.128936,"inputTokens":25787,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.587,"originalCredits":3.967,"afterCompact":false},{"index":55,"time":"22:30","ratio":0.130494,"inputTokens":26099,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.951,"originalCredits":4.879,"afterCompact":false},{"index":56,"time":"22:31","ratio":0.132364,"inputTokens":26473,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.681,"originalCredits":4.202,"afterCompact":false},{"index":57,"time":"22:31","ratio":0.13257,"inputTokens":26514,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.47,"originalCredits":3.675,"afterCompact":false},{"index":58,"time":"22:31","ratio":0.132745,"inputTokens":26549,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.752,"originalCredits":4.381,"afterCompact":false},{"index":59,"time":"22:31","ratio":0.133764,"inputTokens":26753,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.602,"originalCredits":4.004,"afterCompact":false},{"index":60,"time":"22:31","ratio":0.133987,"inputTokens":26797,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.494,"originalCredits":3.735,"afterCompact":false},{"index":61,"time":"22:31","ratio":0.134126,"inputTokens":26825,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.564,"originalCredits":3.91,"afterCompact":false},{"index":62,"time":"22:31","ratio":0.134538,"inputTokens":26908,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.518,"originalCredits":3.795,"afterCompact":false},{"index":63,"time":"22:32","ratio":0.134732,"inputTokens":26946,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.51,"originalCredits":3.776,"afterCompact":false},{"index":64,"time":"22:32","ratio":0.135157,"inputTokens":27031,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.558,"originalCredits":3.895,"afterCompact":false},{"index":65,"time":"22:32","ratio":0.135477,"inputTokens":27095,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.549,"originalCredits":3.874,"afterCompact":false},{"index":66,"time":"22:32","ratio":0.135705,"inputTokens":27141,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.529,"originalCredits":3.823,"afterCompact":false},{"index":67,"time":"22:32","ratio":0.136058,"inputTokens":27212,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.63,"originalCredits":4.074,"afterCompact":false},{"index":68,"time":"22:33","ratio":0.136741,"inputTokens":27348,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.736,"originalCredits":4.34,"afterCompact":false},{"index":69,"time":"22:33","ratio":0.141113,"inputTokens":28223,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.382,"originalCredits":5.956,"afterCompact":false},{"index":70,"time":"22:33","ratio":0.142195,"inputTokens":28439,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.97,"originalCredits":4.926,"afterCompact":false},{"index":71,"time":"22:34","ratio":0.14323,"inputTokens":28646,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.879,"originalCredits":4.697,"afterCompact":false},{"index":72,"time":"22:34","ratio":0.14473,"inputTokens":28946,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.852,"originalCredits":4.631,"afterCompact":false},{"index":73,"time":"22:34","ratio":0.14598,"inputTokens":29196,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.786,"originalCredits":4.464,"afterCompact":false},{"index":74,"time":"22:34","ratio":0.146249,"inputTokens":29250,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.308,"originalCredits":5.769,"afterCompact":false},{"index":75,"time":"22:36","ratio":0.148507,"inputTokens":29701,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.378,"originalCredits":5.945,"afterCompact":false},{"index":76,"time":"22:36","ratio":0.150389,"inputTokens":30078,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.901,"originalCredits":4.753,"afterCompact":false},{"index":77,"time":"22:36","ratio":0.15605,"inputTokens":31210,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.642,"originalCredits":6.606,"afterCompact":false},{"index":78,"time":"22:36","ratio":0.157435,"inputTokens":31487,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.948,"originalCredits":4.871,"afterCompact":false},{"index":79,"time":"22:36","ratio":0.158817,"inputTokens":31763,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.177,"originalCredits":5.442,"afterCompact":false},{"index":80,"time":"22:37","ratio":0.162966,"inputTokens":32593,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.432,"originalCredits":6.079,"afterCompact":false},{"index":81,"time":"22:37","ratio":0.167045,"inputTokens":33409,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.361,"originalCredits":5.901,"afterCompact":false},{"index":82,"time":"22:37","ratio":0.170381,"inputTokens":34076,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.079,"originalCredits":7.697,"afterCompact":false},{"index":83,"time":"22:38","ratio":0.176143,"inputTokens":35229,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.549,"originalCredits":8.872,"afterCompact":false},{"index":84,"time":"22:39","ratio":0.179454,"inputTokens":35891,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.282,"originalCredits":8.206,"afterCompact":false},{"index":85,"time":"22:40","ratio":0.182551,"inputTokens":36510,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.067,"originalCredits":7.667,"afterCompact":false},{"index":86,"time":"22:41","ratio":0.184917,"inputTokens":36983,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.326,"originalCredits":5.815,"afterCompact":false},{"index":87,"time":"22:41","ratio":0.185164,"inputTokens":37033,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.079,"originalCredits":5.198,"afterCompact":false},{"index":88,"time":"22:41","ratio":0.190927,"inputTokens":38185,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.031,"originalCredits":7.577,"afterCompact":false},{"index":89,"time":"22:41","ratio":0.197036,"inputTokens":39407,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.161,"originalCredits":7.904,"afterCompact":false},{"index":90,"time":"22:42","ratio":0.20048,"inputTokens":40096,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.833,"originalCredits":7.081,"afterCompact":false},{"index":91,"time":"22:42","ratio":0.203605,"inputTokens":40721,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.597,"originalCredits":6.493,"afterCompact":false},{"index":92,"time":"22:43","ratio":0.204839,"inputTokens":40968,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.901,"originalCredits":7.253,"afterCompact":false},{"index":93,"time":"22:43","ratio":0.208172,"inputTokens":41634,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.297,"originalCredits":8.243,"afterCompact":false},{"index":94,"time":"22:43","ratio":0.213242,"inputTokens":42648,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.166,"originalCredits":7.915,"afterCompact":false},{"index":95,"time":"22:44","ratio":0.215687,"inputTokens":43137,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.179,"originalCredits":7.948,"afterCompact":false},{"index":96,"time":"22:45","ratio":0.22112,"inputTokens":44224,"outputTokens":null,"usageSource":"transcript-ratio","credits":4.626,"originalCredits":11.565,"afterCompact":false},{"index":97,"time":"22:45","ratio":0.226144,"inputTokens":45229,"outputTokens":null,"usageSource":"transcript-ratio","credits":4.094,"originalCredits":10.236,"afterCompact":false},{"index":98,"time":"22:45","ratio":0.229523,"inputTokens":45905,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.891,"originalCredits":7.228,"afterCompact":false},{"index":99,"time":"22:47","ratio":0.229629,"inputTokens":45926,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.457,"originalCredits":8.642,"afterCompact":false},{"index":100,"time":"22:47","ratio":0.232785,"inputTokens":46557,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.899,"originalCredits":7.248,"afterCompact":false},{"index":101,"time":"22:47","ratio":0.232912,"inputTokens":46582,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.677,"originalCredits":6.692,"afterCompact":false},{"index":102,"time":"22:47","ratio":0.235937,"inputTokens":47187,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.328,"originalCredits":8.319,"afterCompact":false},{"index":103,"time":"22:48","ratio":0.237361,"inputTokens":47472,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.933,"originalCredits":7.333,"afterCompact":false},{"index":104,"time":"22:48","ratio":0.238389,"inputTokens":47678,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.713,"originalCredits":6.782,"afterCompact":false},{"index":105,"time":"22:48","ratio":0.238561,"inputTokens":47712,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.628,"originalCredits":6.569,"afterCompact":false},{"index":106,"time":"22:49","ratio":0.238789,"inputTokens":47758,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.373,"originalCredits":8.433,"afterCompact":false},{"index":107,"time":"22:49","ratio":0.241318,"inputTokens":48264,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.329,"originalCredits":8.322,"afterCompact":false},{"index":108,"time":"22:49","ratio":0.242757,"inputTokens":48551,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.809,"originalCredits":7.024,"afterCompact":false},{"index":109,"time":"22:50","ratio":0.242932,"inputTokens":48586,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.881,"originalCredits":7.202,"afterCompact":false},{"index":110,"time":"22:50","ratio":0.244102,"inputTokens":48820,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.821,"originalCredits":7.052,"afterCompact":false},{"index":111,"time":"22:50","ratio":0.244366,"inputTokens":48873,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.682,"originalCredits":6.706,"afterCompact":false},{"index":112,"time":"22:50","ratio":0.244536,"inputTokens":48907,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.711,"originalCredits":6.776,"afterCompact":false},{"index":113,"time":"22:51","ratio":0.244824,"inputTokens":48965,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.069,"originalCredits":7.673,"afterCompact":false},{"index":114,"time":"22:51","ratio":0.246178,"inputTokens":49236,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.171,"originalCredits":7.927,"afterCompact":false},{"index":115,"time":"22:51","ratio":0.247411,"inputTokens":49482,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.823,"originalCredits":7.058,"afterCompact":false},{"index":116,"time":"22:52","ratio":0.247552,"inputTokens":49510,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.099,"originalCredits":7.747,"afterCompact":false},{"index":117,"time":"22:52","ratio":0.250688,"inputTokens":50138,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.519,"originalCredits":8.798,"afterCompact":false},{"index":118,"time":"22:52","ratio":0.253096,"inputTokens":50619,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.248,"originalCredits":8.12,"afterCompact":false},{"index":119,"time":"22:53","ratio":0.254609,"inputTokens":50922,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.268,"originalCredits":8.17,"afterCompact":false},{"index":120,"time":"22:54","ratio":0.257395,"inputTokens":51479,"outputTokens":null,"usageSource":"transcript-ratio","credits":4.055,"originalCredits":10.138,"afterCompact":false},{"index":121,"time":"22:54","ratio":0.260454,"inputTokens":52091,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.347,"originalCredits":8.367,"afterCompact":false},{"index":122,"time":"22:55","ratio":0.261107,"inputTokens":52221,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.458,"originalCredits":8.644,"afterCompact":false},{"index":123,"time":"22:56","ratio":0.263277,"inputTokens":52655,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.117,"originalCredits":7.792,"afterCompact":false},{"index":124,"time":"22:56","ratio":0.263444,"inputTokens":52689,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.9,"originalCredits":7.249,"afterCompact":false},{"index":125,"time":"22:56","ratio":0.263694,"inputTokens":52739,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.891,"originalCredits":7.228,"afterCompact":false},{"index":126,"time":"22:56","ratio":0.264639,"inputTokens":52928,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.972,"originalCredits":7.431,"afterCompact":false},{"index":127,"time":"22:56","ratio":0.265053,"inputTokens":53011,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.281,"originalCredits":8.203,"afterCompact":false},{"index":128,"time":"22:57","ratio":0.266347,"inputTokens":53269,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.124,"originalCredits":7.809,"afterCompact":false},{"index":129,"time":"22:57","ratio":0.266771,"inputTokens":53354,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.943,"originalCredits":7.358,"afterCompact":false},{"index":130,"time":"22:57","ratio":0.266951,"inputTokens":53390,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.407,"originalCredits":8.517,"afterCompact":false},{"index":131,"time":"22:57","ratio":0.268949,"inputTokens":53790,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.154,"originalCredits":7.885,"afterCompact":false},{"index":132,"time":"22:57","ratio":0.269206,"inputTokens":53841,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.945,"originalCredits":7.363,"afterCompact":false},{"index":133,"time":"22:57","ratio":0.269889,"inputTokens":53978,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.007,"originalCredits":7.517,"afterCompact":false},{"index":134,"time":"22:58","ratio":0.270062,"inputTokens":54012,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.942,"originalCredits":7.356,"afterCompact":false},{"index":135,"time":"22:58","ratio":0.270224,"inputTokens":54045,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.929,"originalCredits":7.323,"afterCompact":false},{"index":136,"time":"22:58","ratio":0.270343,"inputTokens":54069,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.183,"originalCredits":7.958,"afterCompact":false},{"index":137,"time":"22:59","ratio":0.272106,"inputTokens":54421,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.726,"originalCredits":9.314,"afterCompact":false},{"index":138,"time":"22:59","ratio":0.274461,"inputTokens":54892,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.283,"originalCredits":8.207,"afterCompact":false},{"index":139,"time":"23:00","ratio":0.27497,"inputTokens":54994,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.708,"originalCredits":9.271,"afterCompact":false},{"index":140,"time":"23:00","ratio":0.277232,"inputTokens":55446,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.271,"originalCredits":8.177,"afterCompact":false},{"index":141,"time":"23:00","ratio":0.277379,"inputTokens":55476,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.039,"originalCredits":7.599,"afterCompact":false},{"index":142,"time":"23:00","ratio":0.277609,"inputTokens":55522,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.157,"originalCredits":7.893,"afterCompact":false},{"index":143,"time":"23:01","ratio":0.278182,"inputTokens":55636,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.339,"originalCredits":8.347,"afterCompact":false},{"index":144,"time":"23:01","ratio":0.279157,"inputTokens":55831,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.327,"originalCredits":8.317,"afterCompact":false},{"index":145,"time":"23:01","ratio":0.280191,"inputTokens":56038,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.179,"originalCredits":7.947,"afterCompact":false},{"index":146,"time":"23:01","ratio":0.280422,"inputTokens":56084,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.088,"originalCredits":7.721,"afterCompact":false},{"index":147,"time":"23:02","ratio":0.28067,"inputTokens":56134,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.41,"originalCredits":8.524,"afterCompact":false},{"index":148,"time":"23:02","ratio":0.281908,"inputTokens":56382,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.259,"originalCredits":8.148,"afterCompact":false},{"index":149,"time":"23:02","ratio":0.282256,"inputTokens":56451,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.155,"originalCredits":7.888,"afterCompact":false},{"index":150,"time":"23:02","ratio":0.282609,"inputTokens":56522,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.091,"originalCredits":7.728,"afterCompact":false},{"index":151,"time":"23:03","ratio":0.282749,"inputTokens":56550,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.221,"originalCredits":8.052,"afterCompact":false},{"index":152,"time":"23:03","ratio":0.283669,"inputTokens":56734,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.179,"originalCredits":7.947,"afterCompact":false},{"index":153,"time":"23:03","ratio":0.283833,"inputTokens":56767,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.13,"originalCredits":7.825,"afterCompact":false},{"index":154,"time":"23:03","ratio":0.284115,"inputTokens":56823,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.113,"originalCredits":7.783,"afterCompact":false},{"index":155,"time":"23:04","ratio":0.28442,"inputTokens":56884,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.66,"originalCredits":9.149,"afterCompact":false},{"index":156,"time":"23:04","ratio":0.286299,"inputTokens":57260,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.382,"originalCredits":8.454,"afterCompact":false},{"index":157,"time":"23:05","ratio":0.287059,"inputTokens":57412,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.672,"originalCredits":9.179,"afterCompact":false},{"index":158,"time":"23:05","ratio":0.288886,"inputTokens":57777,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.375,"originalCredits":8.438,"afterCompact":false},{"index":159,"time":"23:05","ratio":0.289397,"inputTokens":57879,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.547,"originalCredits":8.867,"afterCompact":false},{"index":160,"time":"23:05","ratio":0.291711,"inputTokens":58342,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.423,"originalCredits":8.558,"afterCompact":false},{"index":161,"time":"23:06","ratio":0.295011,"inputTokens":59002,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.893,"originalCredits":9.733,"afterCompact":false},{"index":162,"time":"23:06","ratio":0.296501,"inputTokens":59300,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.773,"originalCredits":9.431,"afterCompact":false},{"index":163,"time":"23:06","ratio":0.297885,"inputTokens":59577,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.442,"originalCredits":8.604,"afterCompact":false},{"index":164,"time":"23:07","ratio":0.298229,"inputTokens":59646,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.389,"originalCredits":8.472,"afterCompact":false},{"index":165,"time":"23:07","ratio":0.298808,"inputTokens":59762,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.287,"originalCredits":8.217,"afterCompact":false},{"index":166,"time":"23:07","ratio":0.299072,"inputTokens":59814,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.374,"originalCredits":8.435,"afterCompact":false},{"index":167,"time":"23:07","ratio":0.299579,"inputTokens":59916,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.388,"originalCredits":8.47,"afterCompact":false},{"index":168,"time":"23:07","ratio":0.300528,"inputTokens":60106,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.401,"originalCredits":8.503,"afterCompact":false},{"index":169,"time":"23:07","ratio":0.301111,"inputTokens":60222,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.367,"originalCredits":8.419,"afterCompact":false},{"index":170,"time":"23:08","ratio":0.301408,"inputTokens":60282,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.355,"originalCredits":8.388,"afterCompact":false},{"index":171,"time":"23:08","ratio":0.301766,"inputTokens":60353,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.319,"originalCredits":8.298,"afterCompact":false},{"index":172,"time":"23:08","ratio":0.30209,"inputTokens":60418,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.307,"originalCredits":8.269,"afterCompact":false},{"index":173,"time":"23:08","ratio":0.302389,"inputTokens":60478,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.308,"originalCredits":8.271,"afterCompact":false},{"index":174,"time":"23:08","ratio":0.302567,"inputTokens":60513,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.286,"originalCredits":8.214,"afterCompact":false},{"index":175,"time":"23:08","ratio":0.302714,"inputTokens":60543,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.349,"originalCredits":8.372,"afterCompact":false},{"index":176,"time":"23:08","ratio":0.303532,"inputTokens":60706,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.419,"originalCredits":8.548,"afterCompact":false},{"index":177,"time":"23:08","ratio":0.30393,"inputTokens":60786,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.334,"originalCredits":8.335,"afterCompact":false},{"index":178,"time":"23:09","ratio":0.3041,"inputTokens":60820,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.357,"originalCredits":8.392,"afterCompact":false},{"index":179,"time":"23:09","ratio":0.304423,"inputTokens":60885,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.337,"originalCredits":8.343,"afterCompact":false},{"index":180,"time":"23:09","ratio":0.304763,"inputTokens":60953,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.321,"originalCredits":8.302,"afterCompact":false},{"index":181,"time":"23:09","ratio":0.305093,"inputTokens":61019,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.335,"originalCredits":8.337,"afterCompact":false},{"index":182,"time":"23:09","ratio":0.305252,"inputTokens":61050,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.358,"originalCredits":8.395,"afterCompact":false},{"index":183,"time":"23:09","ratio":0.305544,"inputTokens":61109,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.379,"originalCredits":8.447,"afterCompact":false},{"index":184,"time":"23:10","ratio":0.306124,"inputTokens":61225,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.422,"originalCredits":8.555,"afterCompact":false},{"index":185,"time":"23:10","ratio":0.306426,"inputTokens":61285,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.365,"originalCredits":8.413,"afterCompact":false},{"index":186,"time":"23:10","ratio":0.306696,"inputTokens":61339,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.376,"originalCredits":8.439,"afterCompact":false},{"index":187,"time":"23:10","ratio":0.307272,"inputTokens":61454,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.558,"originalCredits":8.895,"afterCompact":false},{"index":188,"time":"23:11","ratio":0.308016,"inputTokens":61603,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.53,"originalCredits":8.825,"afterCompact":false},{"index":189,"time":"23:11","ratio":0.308542,"inputTokens":61708,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.502,"originalCredits":8.755,"afterCompact":false},{"index":190,"time":"23:11","ratio":0.309319,"inputTokens":61864,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.478,"originalCredits":8.695,"afterCompact":false},{"index":191,"time":"23:11","ratio":0.309772,"inputTokens":61954,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.409,"originalCredits":8.523,"afterCompact":false},{"index":192,"time":"23:11","ratio":0.310036,"inputTokens":62007,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.377,"originalCredits":8.443,"afterCompact":false},{"index":193,"time":"23:11","ratio":0.310185,"inputTokens":62037,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.373,"originalCredits":8.432,"afterCompact":false},{"index":194,"time":"23:12","ratio":0.310364,"inputTokens":62073,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.369,"originalCredits":8.422,"afterCompact":false},{"index":195,"time":"23:12","ratio":0.310514,"inputTokens":62103,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.497,"originalCredits":8.744,"afterCompact":false},{"index":196,"time":"23:12","ratio":0.311067,"inputTokens":62213,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.435,"originalCredits":8.588,"afterCompact":false},{"index":197,"time":"23:12","ratio":0.311253,"inputTokens":62251,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.49,"originalCredits":8.724,"afterCompact":false},{"index":198,"time":"23:14","ratio":0.311728,"inputTokens":62346,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.454,"originalCredits":8.635,"afterCompact":false},{"index":199,"time":"23:14","ratio":0.312136,"inputTokens":62427,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.497,"originalCredits":8.743,"afterCompact":false},{"index":200,"time":"23:15","ratio":0.313094,"inputTokens":62619,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.724,"originalCredits":9.309,"afterCompact":false},{"index":201,"time":"23:15","ratio":0.314387,"inputTokens":62877,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.596,"originalCredits":8.99,"afterCompact":false},{"index":202,"time":"23:15","ratio":0.314671,"inputTokens":62934,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.755,"originalCredits":9.388,"afterCompact":false},{"index":203,"time":"23:17","ratio":0.315827,"inputTokens":63165,"outputTokens":null,"usageSource":"transcript-ratio","credits":5.025,"originalCredits":12.562,"afterCompact":false},{"index":204,"time":"23:17","ratio":0.321368,"inputTokens":64274,"outputTokens":null,"usageSource":"transcript-ratio","credits":4.228,"originalCredits":10.569,"afterCompact":false},{"index":205,"time":"23:18","ratio":0.322761,"inputTokens":64552,"outputTokens":null,"usageSource":"transcript-ratio","credits":4.018,"originalCredits":10.046,"afterCompact":false},{"index":206,"time":"23:18","ratio":0.324044,"inputTokens":64809,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.657,"originalCredits":9.142,"afterCompact":false},{"index":207,"time":"23:18","ratio":0.324579,"inputTokens":64916,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.607,"originalCredits":9.018,"afterCompact":false},{"index":208,"time":"23:19","ratio":0.324959,"inputTokens":64992,"outputTokens":null,"usageSource":"transcript-ratio","credits":3.709,"originalCredits":9.273,"afterCompact":false},{"index":209,"time":"23:19","ratio":0.326052,"inputTokens":65210,"outputTokens":null,"usageSource":"transcript-ratio","credits":4.116,"originalCredits":10.291,"afterCompact":false},{"index":210,"time":"23:20","ratio":0.327902,"inputTokens":65580,"outputTokens":null,"usageSource":"transcript-ratio","credits":4.116,"originalCredits":10.29,"afterCompact":false},{"index":211,"time":"23:21","ratio":0.329667,"inputTokens":65933,"outputTokens":null,"usageSource":"transcript-ratio","credits":4.382,"originalCredits":10.954,"afterCompact":false},{"index":212,"time":"23:22","ratio":0.333518,"inputTokens":66704,"outputTokens":null,"usageSource":"transcript-ratio","credits":4.373,"originalCredits":10.932,"afterCompact":false},{"index":213,"time":"23:23","ratio":0.330373,"inputTokens":66075,"outputTokens":null,"usageSource":"transcript-ratio","credits":7.393,"originalCredits":7.393,"afterCompact":false},{"index":214,"time":"23:23","ratio":0.332666,"inputTokens":66533,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.238,"originalCredits":0.238,"afterCompact":false},{"index":215,"time":"23:23","ratio":0.333932,"inputTokens":66786,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.229,"originalCredits":0.229,"afterCompact":false},{"index":216,"time":"23:23","ratio":0.336999,"inputTokens":67400,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.249,"originalCredits":0.249,"afterCompact":false},{"index":217,"time":"23:23","ratio":0.33831,"inputTokens":67662,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.292,"originalCredits":0.292,"afterCompact":false},{"index":218,"time":"23:24","ratio":0.340026,"inputTokens":68005,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.326,"originalCredits":0.326,"afterCompact":false},{"index":219,"time":"23:24","ratio":0.342215,"inputTokens":68443,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.213,"originalCredits":0.213,"afterCompact":false},{"index":220,"time":"23:24","ratio":0.343575,"inputTokens":68715,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.417,"originalCredits":0.417,"afterCompact":false},{"index":221,"time":"23:24","ratio":0.350509,"inputTokens":70102,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.525,"originalCredits":0.525,"afterCompact":false},{"index":222,"time":"23:26","ratio":0.354326,"inputTokens":70865,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.235,"originalCredits":0.235,"afterCompact":false},{"index":223,"time":"23:26","ratio":0.355098,"inputTokens":71020,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.174,"originalCredits":0.174,"afterCompact":false},{"index":224,"time":"23:26","ratio":0.358442,"inputTokens":71688,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.364,"originalCredits":0.364,"afterCompact":false},{"index":225,"time":"23:26","ratio":0.362132,"inputTokens":72426,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.731,"originalCredits":0.731,"afterCompact":false},{"index":226,"time":"23:27","ratio":0.374734,"inputTokens":74947,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.014,"originalCredits":1.014,"afterCompact":false},{"index":227,"time":"23:28","ratio":0.384155,"inputTokens":76831,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.885,"originalCredits":0.885,"afterCompact":false},{"index":228,"time":"23:28","ratio":0.392188,"inputTokens":78438,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.308,"originalCredits":0.308,"afterCompact":false},{"index":229,"time":"23:28","ratio":0.393646,"inputTokens":78729,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.192,"originalCredits":0.192,"afterCompact":false},{"index":230,"time":"23:28","ratio":0.393846,"inputTokens":78769,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.203,"originalCredits":0.203,"afterCompact":false},{"index":231,"time":"23:28","ratio":0.394142,"inputTokens":78828,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.196,"originalCredits":0.196,"afterCompact":false},{"index":232,"time":"23:28","ratio":0.394388,"inputTokens":78878,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.342,"originalCredits":0.342,"afterCompact":false},{"index":233,"time":"23:28","ratio":0.396277,"inputTokens":79255,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.257,"originalCredits":0.257,"afterCompact":false},{"index":234,"time":"23:29","ratio":0.397463,"inputTokens":79493,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.201,"originalCredits":0.201,"afterCompact":false},{"index":235,"time":"23:29","ratio":0.397805,"inputTokens":79561,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.203,"originalCredits":0.203,"afterCompact":false},{"index":236,"time":"23:29","ratio":0.398055,"inputTokens":79611,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.2,"originalCredits":0.2,"afterCompact":false},{"index":237,"time":"23:29","ratio":0.39838,"inputTokens":79676,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.254,"originalCredits":0.254,"afterCompact":false},{"index":238,"time":"23:30","ratio":0.400149,"inputTokens":80030,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.637,"originalCredits":0.637,"afterCompact":false},{"index":239,"time":"23:30","ratio":0.405788,"inputTokens":81158,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.329,"originalCredits":0.329,"afterCompact":false},{"index":240,"time":"23:30","ratio":0.407365,"inputTokens":81473,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.227,"originalCredits":0.227,"afterCompact":false},{"index":241,"time":"23:30","ratio":0.407867,"inputTokens":81573,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.211,"originalCredits":0.211,"afterCompact":false},{"index":242,"time":"23:30","ratio":0.408199,"inputTokens":81640,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.204,"originalCredits":0.204,"afterCompact":false},{"index":243,"time":"23:30","ratio":0.408887,"inputTokens":81777,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.207,"originalCredits":0.207,"afterCompact":false},{"index":244,"time":"23:30","ratio":0.409062,"inputTokens":81812,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.209,"originalCredits":0.209,"afterCompact":false},{"index":245,"time":"23:30","ratio":0.409356,"inputTokens":81871,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.194,"originalCredits":0.194,"afterCompact":false},{"index":246,"time":"23:30","ratio":0.409508,"inputTokens":81902,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.221,"originalCredits":0.221,"afterCompact":false},{"index":247,"time":"23:30","ratio":0.410242,"inputTokens":82048,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.21,"originalCredits":0.21,"afterCompact":false},{"index":248,"time":"23:30","ratio":0.410509,"inputTokens":82102,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.203,"originalCredits":0.203,"afterCompact":false},{"index":249,"time":"23:31","ratio":0.410743,"inputTokens":82149,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.219,"originalCredits":0.219,"afterCompact":false},{"index":250,"time":"23:32","ratio":0.411152,"inputTokens":82230,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.234,"originalCredits":0.234,"afterCompact":false},{"index":251,"time":"23:32","ratio":0.412056,"inputTokens":82411,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.217,"originalCredits":0.217,"afterCompact":false},{"index":252,"time":"23:32","ratio":0.412372,"inputTokens":82474,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.204,"originalCredits":0.204,"afterCompact":false},{"index":253,"time":"23:32","ratio":0.412584,"inputTokens":82517,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.241,"originalCredits":0.241,"afterCompact":false},{"index":254,"time":"23:33","ratio":0.417463,"inputTokens":83493,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.369,"originalCredits":0.369,"afterCompact":false},{"index":255,"time":"23:34","ratio":0.418626,"inputTokens":83725,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.216,"originalCredits":0.216,"afterCompact":false},{"index":256,"time":"23:34","ratio":0.419359,"inputTokens":83872,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.386,"originalCredits":0.386,"afterCompact":false},{"index":257,"time":"23:34","ratio":0.421495,"inputTokens":84299,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.273,"originalCredits":0.273,"afterCompact":false},{"index":258,"time":"23:34","ratio":0.422469,"inputTokens":84494,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.253,"originalCredits":0.253,"afterCompact":false},{"index":259,"time":"23:34","ratio":0.423286,"inputTokens":84657,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.258,"originalCredits":0.258,"afterCompact":false},{"index":260,"time":"23:34","ratio":0.424197,"inputTokens":84839,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.332,"originalCredits":0.332,"afterCompact":false},{"index":261,"time":"23:35","ratio":0.425947,"inputTokens":85189,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.252,"originalCredits":0.252,"afterCompact":false},{"index":262,"time":"23:35","ratio":0.426813,"inputTokens":85363,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.276,"originalCredits":0.276,"afterCompact":false},{"index":263,"time":"23:35","ratio":0.427782,"inputTokens":85556,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.245,"originalCredits":0.245,"afterCompact":false},{"index":264,"time":"09:05","ratio":0.430151,"inputTokens":86030,"outputTokens":null,"usageSource":"transcript-ratio","credits":18.95,"originalCredits":18.95,"afterCompact":false},{"index":265,"time":"09:05","ratio":0.431833,"inputTokens":86367,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.529,"originalCredits":0.529,"afterCompact":false},{"index":266,"time":"09:05","ratio":0.433097,"inputTokens":86619,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.525,"originalCredits":0.525,"afterCompact":false},{"index":267,"time":"09:05","ratio":0.434779,"inputTokens":86956,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.71,"originalCredits":0.71,"afterCompact":false},{"index":268,"time":"09:05","ratio":0.436652,"inputTokens":87330,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.429,"originalCredits":0.429,"afterCompact":false},{"index":269,"time":"09:05","ratio":0.437994,"inputTokens":87599,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.463,"originalCredits":0.463,"afterCompact":false},{"index":270,"time":"09:06","ratio":0.439398,"inputTokens":87880,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.079,"originalCredits":1.079,"afterCompact":false},{"index":271,"time":"09:06","ratio":0.443891,"inputTokens":88778,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.979,"originalCredits":0.979,"afterCompact":false},{"index":272,"time":"09:07","ratio":0.447861,"inputTokens":89572,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.626,"originalCredits":0.626,"afterCompact":false},{"index":273,"time":"09:07","ratio":0.450598,"inputTokens":90120,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.124,"originalCredits":1.124,"afterCompact":false},{"index":274,"time":"09:07","ratio":0.455123,"inputTokens":91025,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.996,"originalCredits":0.996,"afterCompact":false},{"index":275,"time":"09:07","ratio":0.459267,"inputTokens":91853,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.743,"originalCredits":0.743,"afterCompact":false},{"index":276,"time":"09:08","ratio":0.461053,"inputTokens":92211,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.887,"originalCredits":0.887,"afterCompact":false},{"index":277,"time":"09:08","ratio":0.464549,"inputTokens":92910,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.186,"originalCredits":1.186,"afterCompact":false},{"index":278,"time":"09:09","ratio":0.469508,"inputTokens":93902,"outputTokens":null,"usageSource":"transcript-ratio","credits":1.143,"originalCredits":1.143,"afterCompact":false},{"index":279,"time":"09:10","ratio":0.473541,"inputTokens":94708,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.469,"originalCredits":2.469,"afterCompact":false},{"index":280,"time":"09:10","ratio":0.486149,"inputTokens":97230,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.74,"originalCredits":0.74,"afterCompact":false},{"index":281,"time":"09:10","ratio":0.487966,"inputTokens":97593,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.764,"originalCredits":0.764,"afterCompact":false},{"index":282,"time":"09:11","ratio":0.4904,"inputTokens":98080,"outputTokens":null,"usageSource":"transcript-ratio","credits":2.242,"originalCredits":2.242,"afterCompact":false},{"index":283,"time":"09:11","ratio":0.500399,"inputTokens":100080,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.484,"originalCredits":0.484,"afterCompact":false},{"index":284,"time":"09:11","ratio":0.500611,"inputTokens":100122,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.5,"originalCredits":0.5,"afterCompact":false},{"index":285,"time":"09:11","ratio":0.500887,"inputTokens":100177,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.516,"originalCredits":0.516,"afterCompact":false},{"index":286,"time":"09:11","ratio":0.501278,"inputTokens":100256,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.501,"originalCredits":0.501,"afterCompact":false},{"index":287,"time":"09:12","ratio":0.50217,"inputTokens":100434,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.705,"originalCredits":0.705,"afterCompact":false},{"index":288,"time":"09:12","ratio":0.503824,"inputTokens":100765,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.72,"originalCredits":0.72,"afterCompact":false},{"index":289,"time":"09:12","ratio":0.506134,"inputTokens":101227,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.673,"originalCredits":0.673,"afterCompact":false},{"index":290,"time":"09:13","ratio":0.50723,"inputTokens":101446,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.808,"originalCredits":0.808,"afterCompact":false},{"index":291,"time":"09:13","ratio":0.509261,"inputTokens":101852,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.669,"originalCredits":0.669,"afterCompact":false},{"index":292,"time":"09:14","ratio":0.510918,"inputTokens":102184,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.945,"originalCredits":0.945,"afterCompact":false},{"index":293,"time":"09:14","ratio":0.513574,"inputTokens":102715,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.486,"originalCredits":0.486,"afterCompact":false},{"index":294,"time":"09:14","ratio":0.514113,"inputTokens":102823,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.761,"originalCredits":0.761,"afterCompact":false},{"index":295,"time":"09:14","ratio":0.516107,"inputTokens":103221,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.683,"originalCredits":0.683,"afterCompact":false},{"index":296,"time":"09:14","ratio":0.517306,"inputTokens":103461,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.487,"originalCredits":0.487,"afterCompact":false},{"index":297,"time":"09:15","ratio":0.517824,"inputTokens":103565,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.856,"originalCredits":0.856,"afterCompact":false},{"index":298,"time":"09:15","ratio":0.520261,"inputTokens":104052,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.7,"originalCredits":0.7,"afterCompact":false},{"index":299,"time":"09:15","ratio":0.521552,"inputTokens":104310,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.509,"originalCredits":0.509,"afterCompact":false},{"index":300,"time":"09:16","ratio":0.522471,"inputTokens":104494,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.879,"originalCredits":0.879,"afterCompact":false},{"index":301,"time":"09:16","ratio":0.524663,"inputTokens":104933,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.652,"originalCredits":0.652,"afterCompact":false},{"index":302,"time":"09:16","ratio":0.525705,"inputTokens":105141,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.717,"originalCredits":0.717,"afterCompact":false},{"index":303,"time":"09:16","ratio":0.527142,"inputTokens":105428,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.514,"originalCredits":0.514,"afterCompact":false},{"index":304,"time":"09:16","ratio":0.527675,"inputTokens":105535,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.553,"originalCredits":0.553,"afterCompact":false},{"index":305,"time":"09:17","ratio":0.528074,"inputTokens":105615,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.503,"originalCredits":0.503,"afterCompact":false},{"index":306,"time":"09:17","ratio":0.528939,"inputTokens":105788,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.825,"originalCredits":0.825,"afterCompact":false},{"index":307,"time":"09:17","ratio":0.531283,"inputTokens":106257,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.658,"originalCredits":0.658,"afterCompact":false},{"index":308,"time":"09:17","ratio":0.532214,"inputTokens":106443,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.553,"originalCredits":0.553,"afterCompact":false},{"index":309,"time":"09:17","ratio":0.532634,"inputTokens":106527,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.505,"originalCredits":0.505,"afterCompact":false},{"index":310,"time":"09:17","ratio":0.533091,"inputTokens":106618,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.573,"originalCredits":0.573,"afterCompact":false},{"index":311,"time":"09:17","ratio":0.53365,"inputTokens":106730,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.531,"originalCredits":0.531,"afterCompact":false},{"index":312,"time":"09:17","ratio":0.533995,"inputTokens":106799,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.51,"originalCredits":0.51,"afterCompact":false},{"index":313,"time":"09:17","ratio":0.534167,"inputTokens":106833,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.515,"originalCredits":0.515,"afterCompact":false},{"index":314,"time":"09:17","ratio":0.535007,"inputTokens":107001,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.772,"originalCredits":0.772,"afterCompact":false},{"index":315,"time":"09:18","ratio":0.537597,"inputTokens":107519,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.775,"originalCredits":0.775,"afterCompact":false},{"index":316,"time":"09:18","ratio":0.539018,"inputTokens":107804,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.535,"originalCredits":0.535,"afterCompact":false},{"index":317,"time":"09:18","ratio":0.53933,"inputTokens":107866,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.548,"originalCredits":0.548,"afterCompact":false},{"index":318,"time":"09:18","ratio":0.539708,"inputTokens":107942,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.54,"originalCredits":0.54,"afterCompact":false},{"index":319,"time":"09:18","ratio":0.540042,"inputTokens":108008,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.519,"originalCredits":0.519,"afterCompact":false},{"index":320,"time":"09:19","ratio":0.540265,"inputTokens":108053,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.839,"originalCredits":0.839,"afterCompact":false},{"index":321,"time":"09:20","ratio":0.542565,"inputTokens":108513,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.753,"originalCredits":0.753,"afterCompact":false},{"index":322,"time":"09:22","ratio":0.544058,"inputTokens":108812,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.528,"originalCredits":0.528,"afterCompact":false},{"index":323,"time":"09:22","ratio":0.544372,"inputTokens":108874,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.591,"originalCredits":0.591,"afterCompact":false},{"index":324,"time":"09:22","ratio":0.545052,"inputTokens":109010,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.524,"originalCredits":0.524,"afterCompact":false},{"index":325,"time":"09:22","ratio":0.547486,"inputTokens":109497,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.981,"originalCredits":0.981,"afterCompact":false},{"index":326,"time":"09:22","ratio":0.549754,"inputTokens":109951,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.552,"originalCredits":0.552,"afterCompact":false},{"index":327,"time":"09:23","ratio":0.551369,"inputTokens":110274,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.757,"originalCredits":0.757,"afterCompact":false},{"index":328,"time":"09:34","ratio":0.552741,"inputTokens":110548,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.598,"originalCredits":0.598,"afterCompact":false},{"index":329,"time":"09:34","ratio":0.553523,"inputTokens":110705,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.679,"originalCredits":0.679,"afterCompact":false},{"index":330,"time":"09:35","ratio":0.554695,"inputTokens":110939,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.744,"originalCredits":0.744,"afterCompact":false},{"index":331,"time":"09:36","ratio":0.556167,"inputTokens":111233,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.712,"originalCredits":0.712,"afterCompact":false},{"index":332,"time":"09:36","ratio":0.557416,"inputTokens":111483,"outputTokens":null,"usageSource":"transcript-ratio","credits":0.654,"originalCredits":0.654,"afterCompact":false}],"identity":{"sumAttributed":19853789,"billedInputTokens":19853789,"absDiff":0,"ok":true},"pluginVersion":"2.7.4","selfCmd":"\"C:\\Users\\TouchGo\\.qoder-cn\\plugins\\cache\\qoder-marketplace\\qoder-credits-inspector\\2.7.4\\bin\\credits-inspector.cmd\" cli","linkHealth":{"matched":0,"requests":332,"coverage":0,"usageSource":"transcript-ratio","breakdown":{"proxy":0,"transcriptTokens":0,"transcriptRatio":332},"logRecords":0,"lastRecordAt":null,"recentRequests":0,"proxyErrors":null,"clientAbort":null,"clientAbortMaxMs":null,"upstreamRejected":null,"aborted":null,"truncated":null,"noUsage":null,"slowestMs":null,"supersededFailure":null},"subagents":{"scanned":true,"dir":"C:\\Users\\TouchGo\\.qoder-cn\\projects\\D--TouchGoCore\\4aeb79eb-5a0f-43ff-a744-4df540cc955d\\subagents","probedPaths":["C:\\Users\\TouchGo\\.qoder-cn\\projects\\D--TouchGoCore\\4aeb79eb-5a0f-43ff-a744-4df540cc955d\\subagents","C:\\Users\\TouchGo\\.qoder-cn\\projects\\4aeb79eb-5a0f-43ff-a744-4df540cc955d\\subagents"],"reason":"ok","count":4,"items":[{"agentId":"ageneral-purpose-50eb348f34c58ea1","agentType":"general-purpose","description":"审查 pay 与四链集成","toolUseId":"call_e2e6c6d93e3d4260b87c8438","roundTrips":28,"billedInputTokens":611142,"peakContextRatio":0.1813,"credits":58.966,"originalCredits":147.415,"error":null},{"agentId":"ageneral-purpose-7a1684e9cbd4ddd1","agentType":"general-purpose","description":"独立审计 pay 残留漏洞","toolUseId":"call_c982e2616f7c4f3ba32cdc3a","roundTrips":23,"billedInputTokens":458580,"peakContextRatio":0.1384,"credits":44.346,"originalCredits":110.864,"error":null},{"agentId":"ageneral-purpose-a94ae2abda871267","agentType":"general-purpose","description":"审查 instagram 历史代码","toolUseId":"call_ec910897a73d4f3abf8b6129","roundTrips":28,"billedInputTokens":264945,"peakContextRatio":0.0658,"credits":25.417,"originalCredits":63.543,"error":null},{"agentId":"ageneral-purpose-06d46a7009c4acce","agentType":"general-purpose","description":"审查 facebook 历史代码","toolUseId":"call_696bc4750b2a431b8e10fb3b","roundTrips":10,"billedInputTokens":81026,"peakContextRatio":0.0546,"credits":13.065,"originalCredits":32.662,"error":null}],"totals":{"roundTrips":89,"billedInputTokens":1415693,"credits":141.794,"originalCredits":354.484},"combined":{"roundTrips":421,"billedInputTokens":21269482,"credits":798.945,"originalCredits":1860.641}},"manual":null,"artifacts":{"report":"report.json","canvas":"1、审查pay集成的tron_ton_bsc_s__4aeb79eb.canvas.tsx"}} as unknown as Report;

function human(n: number): string {
  if (!isFinite(n)) return "—";
  const abs = Math.abs(n);
  if (abs >= 1e9) return (n / 1e9).toFixed(2) + "B";
  if (abs >= 1e6) return (n / 1e6).toFixed(2) + "M";
  if (abs >= 1e3) return (n / 1e3).toFixed(1) + "K";
  return String(Math.round(n));
}

function pct(x: number): string {
  return (x * 100).toFixed(1) + "%";
}

// —— 可用性口径：把 report.availability 翻成人话，并决定某个数字该显示值、「≈」还是「—」——
const AVAIL_LABEL: Record<string, string> = {
  measured: "实测",
  derived: "推导",
  fallback: "回退",
  manual: "手工",
  unavailable: "不可用",
};

const SOURCE_LABEL: Record<string, string> = {
  "desktop-rich": "桌面端富转录",
  "ide-lite": "IDE 端精简转录",
  unknown: "来源未知",
};

function availOf(av: Availability | undefined, key: keyof Availability, dflt: Avail = "measured"): Avail {
  return (av?.[key] as Avail | undefined) ?? dflt;
}

/** 段标题旁的性质标注：实测不标（默认就是实测），其余标出来。 */
function availTag(av: Availability | undefined, key: keyof Availability, dflt: Avail = "measured"): string {
  const v = availOf(av, key, dflt);
  return v === "measured" ? "" : `（${AVAIL_LABEL[v]}）`;
}

/** 不可用的量显示「—」而不是 0：IDE 端的 0 是「读不到」，不是「没发生」。 */
function orDash(
  v: number,
  av: Availability | undefined,
  key: keyof Availability,
  fmt: (n: number) => string = String
): string {
  return availOf(av, key) === "unavailable" ? "—" : fmt(v);
}

function kindLabel(kind: string): string {
  switch (kind) {
    case "read":
      return "读取";
    case "write":
      return "写入";
    case "search":
      return "搜索";
    case "shell":
      return "命令";
    default:
      return "其他";
  }
}

function shortTime(iso: string | null): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return String(iso);
  const p = (v: number) => String(v).padStart(2, "0");
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

export default function SessionTokensReport() {
  if (!REPORT) {
    return (
      <ReportShell width="wide" ariaLabel="会话 token 消耗截析">
        <Stack gap="component">
          <H1>会话 token 消耗截析</H1>
          <Text tone="secondary">报告数据尚未注入。请在会话中触发一次 Stop 钩子，或运行 CLI 生成 report.json。</Text>
        </Stack>
      </ReportShell>
    );
  }

  const r = REPORT;
  const s = r.session;
  const c = r.context;
  const t = r.totals;
  const av = r.availability;
  const manual = r.manual ?? null;
  const sourceLabel = SOURCE_LABEL[r.source ?? "unknown"] ?? "来源未知";
  // 有没有 usage 决定整份报告是「真值/推导」还是「一律 —」。手工回填的 credits 不算有 usage，
  // 否则下面按 ratio 推导的图表会照画一堆 0。
  const hasUsage = availOf(av, "roundTrips") === "measured";
  const manualCredits = manual?.credits ?? null;

  // 报告里出现的命令一律用数据层注入的插件入口（Windows 上是自带启动器的绝对路径，免装 Node）。
  // 旧报告没有这个字段时回退原写法，不比改动前更差。
  const CLI = r.selfCmd || "node scripts/cli.mjs";

  // credits 覆盖范围：credits 与「计费输入总量」并排放在头部，读者一除就得到单价。
  // 混合会话里 credits 只来自转录路径的那几笔（实测某会话 413 笔里只有 1 笔带 credits），
  // 部分覆盖不说出来 = 把局部真值当全量展示。
  const cov = r.creditsCoverage ?? null;
  const covPartial = !!(cov && !cov.full && cov.roundTrips > 0);
  const covNote = covPartial ? ` · 仅覆盖 ${cov!.trips}/${cov!.roundTrips} 笔往返（占计费输入 ${pct(cov!.tokenShare)}）` : "";

  // 缓存命中：代理记录里一直带着 cachedTokens，此前采集了却从不呈现。
  const cachedMeasured = availOf(av, "cachedTokens") === "measured";
  const cachedText = cachedMeasured
    ? `输入 ${human(t.billedInputTokens)}，其中缓存命中 ${human(t.cachedTokens ?? 0)}（${pct(t.cachedShare ?? 0)}，${t.cachedTrips ?? 0} 笔往返带缓存）——供应商对重复叠加的前缀打折，故按 token 数的节省大于按计费的节省。`
    : "";

  // 代理告警只在「失败记录仍然成立」时才报：status.json 只写不清，陈旧失败会把用户推去改
  // 本来正确的 Base URL，越修越坏；而同一份报告里的 lastRecordAt / matched 是它在跑的硬证据。
  const proxyAlarm = r.proxy?.status && !r.proxy.statusSuperseded ? r.proxy.status : null;
  const proxyAlive = !proxyAlarm && (r.proxy?.logRecords ?? 0) > 0;

  // 官方 UI 一场会话扣费 = 主链 + 子代理，故有子代理时 headline 必须给 combined，否则比 UI 少一截
  const creditScope =
    r.subagents && r.subagents.count > 0
      ? {
          credits: r.subagents.combined.credits,
          note: `主链 ${t.credits} + 子代理 ${r.subagents.totals.credits} · 原始 ${r.subagents.combined.originalCredits}`,
        }
      : { credits: t.credits, note: `原始 ${t.originalCredits}（实测）` };

  const headline: MetricItem[] = [
    {
      label: "计费输入总量",
      value: hasUsage ? human(t.billedInputTokens) : "—",
      description: hasUsage
        ? availOf(av, "tokens") === "measured"
          ? `Σ prompt_tokens（实测真值）${covPartial ? ` · 覆盖全部 ${cov!.roundTrips} 笔往返` : ""}`
          : `Σ 上下文 × 往返${availTag(av, "tokens", "derived")}${covPartial ? ` · 覆盖全部 ${cov!.roundTrips} 笔往返` : ""}`
        : "转录无 usage",
    },
    {
      label: "当前上下文",
      value: hasUsage ? human(t.netContextTokens) : "—",
      description: hasUsage
        ? `峰值 ${pct(c.peakContextRatio)}${s.compactions ? "（压缩后起算）" : ""}`
        : "转录无 usage",
    },
    {
      label: "重发放大",
      value: hasUsage ? `${t.amplification}×` : "—",
      description: `往返 ${hasUsage ? `${s.roundTrips} 次` : "—"}`,
    },
    {
      label: "Credits",
      // 代理路径（自定义模型）token 是实测真值但 credits 无来源：hasUsage 为真也要显示「—」，
      // 否则 t.credits=0 会被读成「这次没花钱」。
      value:
        manualCredits != null
          ? `${manualCredits}`
          : hasUsage && availOf(av, "credits") !== "unavailable"
            ? `${creditScope.credits}`
            : "—",
      description:
        manualCredits != null
          ? `官方 UI 手工录入 · 本地 ${creditScope.credits}`
          : hasUsage && availOf(av, "credits") !== "unavailable"
            ? `${creditScope.note}${covNote}`
            : r.usageSource === "proxy" || r.usageSource === "transcript-tokens" || r.usageSource === "mixed"
              ? "自定义模型（BYOK）不经 Qoder 计费网关，无 credits 真值"
              : hasUsage
                ? "转录带 usage 但没有 credits 字段，本地无计费真值"
                : "本地不可用，见下方告警",
    },
  ];

  const pieData = r.byCategory.map((x) => ({ label: x.label, value: x.tokens }));

  // 系统提示词是报告里少数「用户能直接动」的一项：它每轮被完整重发，总量 = 每请求规模 × 往返数，
  // 而它的大小由装了多少插件与技能决定。占比小时不必啰嗦。
  const sysCat = r.byCategory.find((x) => x.key === "system");
  const sysNote =
    hasUsage && sysCat && sysCat.share >= 0.1
      ? ` 系统提示词占 ${pct(sysCat.share)}（${human(c.systemPromptTokens)}/请求 × ${s.roundTrips} 次往返 = ${human(sysCat.tokens)}）：它每轮都被完整重发，大小由你装了多少插件与技能决定，精简技能能直接压低这一项——本插件自己的技能描述也常驻在里面（约 170 token）。`
      : "";

  const tools = r.byTool.slice(0, 8);
  const toolCategories = tools.map((x) => x.tool);
  const toolSeries = [{ name: "重发 tokens", data: tools.map((x) => x.tokens) }];
  // 「单次」= 平均每次调用带来的重发 token。总量榜会埋掉次数少但每次极贵的工具：
  // 实测 Read 只 29 次却吃 335 万，单次是 Edit 的 2.6 倍。
  const toolRows = tools.map((x) => [
    x.tool,
    human(x.tokens),
    pct(x.share),
    String(x.calls),
    x.perCall == null ? "—" : human(x.perCall),
  ]);

  const files = r.byFile.slice(0, 20);
  const fileRows = files.map((f) => [
    f.label,
    kindLabel(f.kind),
    human(f.billed),
    pct(f.share),
    f.perRead == null ? "—" : human(f.perRead),
    String(f.trips),
    String(f.reads),
  ]);

  const byReq = r.byRequest ?? [];
  const reqLabels = byReq.map((x, i) => x.time || `#${x.index ?? i + 1}`);
  const reqInputs = byReq.map((x) => x.inputTokens);
  const reqCredits = byReq.map((x) => x.credits);
  // credits 只在覆盖全部往返时才画曲线：部分覆盖（BYOK 混合会话实测 413 笔里 1 笔有 credits）
  // 画出来是一地零，既读不出形状，还会暗示「其余往返没花钱」。
  const showCreditsChart = hasUsage && !covPartial && availOf(av, "credits") !== "unavailable";

  // 按类别下钻：类别小计行（accent/neutral）+ 其工具/文件明细行（default），用 rowTone 分组着色。
  const catDetail = r.byCategoryDetail ?? [];
  const detailRows: string[][] = [];
  const detailTones: ("accent" | "neutral" | "default")[] = [];
  for (const cc of catDetail) {
    const hasKids = cc.children.length > 0;
    detailRows.push([cc.label, "—", hasKids ? "小计" : "（整体，无工具/文件归属）", human(cc.tokens), pct(cc.share), "100%", "—"]);
    detailTones.push(hasKids ? "accent" : "neutral");
    for (const k of cc.children) {
      detailRows.push(["", k.tool || "—", k.label, human(k.tokens), pct(k.share), pct(k.catShare), String(k.trips)]);
      detailTones.push("default");
    }
  }

  // 子代理账：Agent 派发的子代理消耗不在主链里，单独一段呈现（无子代理则整段不渲染）。
  const sub = r.subagents;
  const subItems = sub?.items ?? [];
  const subRows: string[][] = [];
  const subTones: ("accent" | "default")[] = [];
  for (const a of subItems) {
    subRows.push([
      a.description || a.agentId,
      a.agentType || "—",
      String(a.roundTrips),
      human(a.billedInputTokens),
      pct(a.peakContextRatio),
      String(a.credits),
      String(a.originalCredits),
      a.error || "—",
    ]);
    subTones.push("default");
  }
  if (sub && subItems.length > 0) {
    subRows.push([
      "合计（主链 + 子代理）",
      "—",
      String(sub.combined.roundTrips),
      human(sub.combined.billedInputTokens),
      "—",
      String(sub.combined.credits),
      String(sub.combined.originalCredits),
      `主链 ${t.credits} / 子代理 ${sub.totals.credits}`,
    ]);
    subTones.push("accent");
  }

  // 压缩单笔成本：输入/摘要一律取数据层归一后的生效值（有代理实测就是实测，否则是客户端自估），
  // 并用「口径」列把两者分开——混着显示会让自估值被当成实测。
  const ccost = r.compactionCost;
  const ccItems = ccost?.items ?? [];
  const compactRows: string[][] = ccItems.map((e) => [
    `#${e.index}`,
    shortTime(e.at),
    e.trigger || "—",
    e.effectiveInputTokens == null ? "—" : human(e.effectiveInputTokens),
    e.effectiveOutputTokens == null ? "—" : human(e.effectiveOutputTokens),
    e.nextInputTokens == null ? "—" : human(e.nextInputTokens),
    e.savedTokens == null ? "—" : human(e.savedTokens),
    e.proxy && e.proxy.ms != null ? `${Math.round(e.proxy.ms / 1000)}s` : "—",
    e.proxy ? "实测" : "自估",
  ]);

  // IDE 端仅剩的真值：工具调用次数与文件读取次数（不依赖 usage，两种转录都写 tool_use 块）。
  const residueRows: string[][] = [];
  if (!hasUsage) {
    for (const x of r.byTool.filter((v) => v.calls > 0).sort((a, b) => b.calls - a.calls).slice(0, 15)) {
      residueRows.push([x.tool, "工具", String(x.calls), "—", "—"]);
    }
    for (const f of r.byFile.filter((v) => v.reads > 0).sort((a, b) => b.reads - a.reads).slice(0, 15)) {
      residueRows.push([f.label, kindLabel(f.kind), "—", String(f.reads), String(f.trips)]);
    }
  }

  // A1 三值：一次带 usage 的往返都没有时 0 <= max(2,0) 恒成立，会把「没数据」判成「校验通过」，
  // 故 breakdown.mjs 在 reqCount===0 时给 null；这里必须显示「不适用」而不是绿色通过。
  const identityText =
    r.identity.ok == null
      ? "恒等式 A1 不适用（本转录没有一次带 usage 的往返）。"
      : r.identity.ok
        ? `归因覆盖 ${pct(t.coverage)}${availTag(av, "categoryShare", "derived")}，恒等式 A1 通过（|Δ|=${r.identity.absDiff}）。`
        : `归因覆盖 ${pct(t.coverage)}，恒等式 A1 未通过（|Δ|=${r.identity.absDiff}）。`;
  // 窗口读不到时是静默回退的 200000，而所有 token 数字都乘它 —— 必须显式标 ≈ 与来源。
  const cwIsFallback = availOf(av, "contextWindow") === "fallback";
  const cwText = cwIsFallback ? `≈${human(c.contextWindow)}（回退值）` : human(c.contextWindow);
  // 当前占用头条：以最新上下文（currentContextTokens）为准；段内单调递增 ⇒ 旧版峰值与当前恒等，故合并为一条。
  const curCtx = c.currentContextTokens ?? c.peakContextTokens;
  const netAdv = c.netUserAdvice || c.peakUserAdvice || c.peakAdvice || null;
  const limitSrcLabel =
    c.userContextLimitSource === "manual" ? "手工"
    : c.userContextLimitSource === "config" ? "配置"
    : c.userContextLimitSource === "derived" ? "实测反推"
    : "默认";
  // 阈值来源非 manual/config/derived ⇒ 用的是内置兜底 200K，不是用户在 Qoder 设的真值，需显式提示如何改。
  const limitIsDefault = c.userContextLimitSource !== "manual" && c.userContextLimitSource !== "config" && c.userContextLimitSource !== "derived";
  // 上下文对比表：把旧版挤成一段小字的「界面显示 / 自动压缩实况 / 历史高点」拆成可扫读的行，无数据不占位。
  const ctxRows: string[][] = [];
  if (c.userContextLimit != null && Number.isFinite(c.contextWindow)) {
    ctxRows.push(
      c.contextWindow > c.userContextLimit
        ? ["Qoder 界面进度条", pct(c.netWindowRatio ?? c.peakContextRatio), `按模型物理窗口 ${cwText} 算，比你真实占比低约 ${(c.contextWindow / c.userContextLimit).toFixed(1)} 倍——界面显得偏空、有迷惑性，别信它`]
        : ["Qoder 界面进度条", pct(c.netWindowRatio ?? c.peakContextRatio), `按模型窗口 ${cwText} 算，与你实设上限一致，显示无偏差`],
    );
  }
  if (c.observedAutoCompactions != null && c.observedAutoCompactions > 0 && c.observedAutoTriggerTokens != null) {
    ctxRows.push(["自动压缩实况", `自动 ${c.observedAutoCompactions} 次`, `触发点在 ~${human(c.observedAutoTriggerTokens)}（窗口的 ${pct(c.autoTriggerRatio ?? 0)}）`]);
  } else if (s.compactions > 0) {
    ctxRows.push(["压缩实况", `手动 ${s.compactions} 次`, "本场未见自动压缩，均为你手动触发"]);
  }
  if (c.peakSessionNotable) {
    ctxRows.push(["压缩前历史高点", `${pct(c.peakSessionRatio ?? 0)}（${human(c.peakSessionTokens ?? 0)}）`, "本场曾达到的最高占用"]);
  }

  // 链路健康度：把 --request-log 的逐笔归因压成一行结论（明细属于排障，留在 CLI）。
  // 分两档：本会话确实走在代理链路上（proxy/mixed）才印「最近 N 笔」的失败统计与排障命令；
  // token 全来自转录的会话里，那些统计说的是别的会话，印成 warning + 命令是噪音，只留覆盖率与最近记录时间。
  // 纯官方模型用户从没配过代理，整行不渲染。
  const lhRaw = r.linkHealth ?? null;
  const lh = lhRaw && (r.proxy?.configured || lhRaw.matched > 0) ? lhRaw : null;
  const lhOnPath = !!lh && (lh.matched > 0 || lh.usageSource === "proxy" || lh.usageSource === "mixed");
  const lhErrors = lh ? lh.proxyErrors ?? 0 : 0;
  const lhBad = lhOnPath && lhErrors > 0;
  const lhTone: "info" | "warning" = lhBad ? "warning" : "info";
  // 三档拆分一行说清：代理实测 / 转录 input_tokens / 转录 ratio×window 各多少笔。
  // 旧报告（v3）无 breakdown 字段时退回到只报 matched。
  const lhBd = lh?.breakdown ?? null;
  const lhBdText = lhBd
    ? `拆分：代理 ${lhBd.proxy} 笔 · 转录 input_tokens ${lhBd.transcriptTokens} 笔 · 转录 ratio×窗口 ${lhBd.transcriptRatio} 笔`
    : "";
  const lhSrcNote = !lh
    ? ""
    : lh.usageSource === "proxy"
      ? "：全部为供应商实测"
      : lh.usageSource === "transcript-tokens"
        ? "：本会话 token 全来自转录 input_tokens（BYOK，代理未在链路上）"
        : lh.usageSource === "transcript-ratio"
          ? "：本会话 token 由 ratio×窗口推导（官方模型）"
          : lh.usageSource === "mixed"
            ? "：多源混合，以下拆分列为准"
            : lh.usageSource === "transcript"
              ? "：本会话 token 全部来自转录，代理不在链路上"
              : "";
  const lhText = !lh
    ? ""
    : [
        lh.requests > 0
          ? `代理覆盖 ${lh.matched}/${lh.requests} 笔往返（${pct(lh.coverage)}）${lhSrcNote}`
          : "本会话没有带 usage 的往返，代理无从覆盖",
        lhBdText,
        lh.lastRecordAt ? `代理最近记录 ${shortTime(lh.lastRecordAt)}` : proxyAlive ? "代理有记录但无时间戳" : "代理从未记到流量",
        lhOnPath && lh.recentRequests
          ? `最近 ${lh.recentRequests} 笔：代理失败 ${lh.proxyErrors} · 上游报错 ${lh.upstreamRejected} · 客户端提前断开 ${lh.aborted} · 成功但无 usage ${lh.noUsage}${
              lh.slowestMs != null ? ` · 最慢 ${Math.round(lh.slowestMs / 1000)}s` : ""
            }`
          : lhOnPath
            ? "请求诊断日志为空（运行中的代理是旧版，或还没记到）"
            : "",
        lhBad ? `有「代理失败」= 请求没出得去或代理自己抛了，跑 ${CLI} --request-log 看归因` : "",
        !lhOnPath && lhErrors > 0 ? `代理另有 ${lhErrors} 笔失败，属于走代理的那些会话` : "",
        lh.supersededFailure
          ? `${shortTime(lh.supersededFailure.at ?? null)} 那条「拉起失败（${lh.supersededFailure.error}）」已被之后的流量推翻，无需处理`
          : "",
      ]
        .filter(Boolean)
        .join("。") + "。";

  return (
    <ReportShell width="wide" ariaLabel="会话 token 消耗截析">
      <Stack gap="sectionCompact">
        <header>
          <Stack gap="component">
            <H1>会话 token 消耗截析</H1>
            <Text tone="secondary">
              {s.model || "未知模型"}
              {s.modelSource === "manual" ? "（手工录入）" : ""} · {sourceLabel} · 会话{" "}
              {String(s.id || "").slice(0, 8)}
              {s.title ? `「${s.title}」` : ""} · {shortTime(s.startedAt)} →{" "}
              {shortTime(s.endedAt)} · 压缩{" "}
              {availOf(av, "compactions") === "unavailable" ? "—" : `${s.compactions} 次`}
              {r.usageSource ? ` · usage 来源 ${
                r.usageSource === "proxy"
                  ? "代理实测"
                  : r.usageSource === "transcript-tokens"
                    ? "转录 input_tokens"
                    : r.usageSource === "transcript-ratio"
                      ? "转录 ratio×窗口"
                      : r.usageSource === "mixed"
                        ? "多源混合"
                        : "转录"
              }` : ""}
              {r.pluginVersion ? ` · v${r.pluginVersion}` : ""} · schema v{r.schemaVersion}
            </Text>
            <MetricsGrid variant="header" columns={4} items={headline} />
          </Stack>
        </header>

        {!hasUsage && r.source === "desktop-rich" && (
          <Callout tone="danger" title="本报告数值不可用：自定义模型（BYOK）不经 Qoder 计费网关">
            转录是桌面端富布局，但 assistant entry 里没有 message.usage——自定义模型（如千问
            tokenplan）的响应不经 Qoder 计费网关，credits / context_usage_ratio 无来源，提供商返回的
            usage 客户端也不落盘（已实测扫过 ~/.qoder-cn、~/.qoder、~/.qoder-cli 与 %APPDATA%\QoderCN）。
            本地补救（改一个配置文件即可，免装 Node、之后全自动）：编辑 {"`~/.qoder-credits-proxy/config.json`"}，
            把 {"`upstream`"} 填成你的供应商根地址（= Base URL 去掉结尾 /v1），再把自定义模型 Base URL 的 host:port
            换成 {`127.0.0.1:${r.proxy?.port ?? 49787}`}（路径保留），新开一个会话即自动拉起代理、按 message.id
            精确 join 出实测 token（credits 仍无真值）。装了 Node 也可用 {"`--setup-proxy`"} / {"`--check-proxy`"} 一步到位。
            手工回填通道同样可用：
            {"`.qoder-credits/overrides/<sessionId>.json`"}。
          </Callout>
        )}

        {!hasUsage && r.source !== "desktop-rich" && (
          <Callout tone="danger" title="本报告数值不可用：转录不含 message.usage">
            这是 {sourceLabel}（IDE 端客户端）。它的转录只写 session_meta / user / assistant / progress 四种 entry，
            message.usage 整个字段不存在，也不落到本地任何其它文件（已实测扫过 ~/.qoder-cn、~/.qoder、
            ~/.qoder-cli 与 %APPDATA%\QoderCN）。因此 Credits、token 与各类占比一律显示「—」而不是 0 ——
            0 会被误读成「这次没花钱」。真值只有官方 UI 有：把它填进{" "}
            {"`.qoder-credits/overrides/<sessionId>.json`"} 后重跑，上方会出现「官方 UI 真值」一段并与本地并列对账。
          </Callout>
        )}

        {manual && (
          <Callout tone="success" title="官方 UI 真值（手工录入，未覆盖任何本地数字）">
            Credits {manual.credits ?? "—"} · 原价 {manual.originalCredits ?? "—"}
            {manual.model ? ` · 模型 ${manual.model}` : ""}
            {manual.durationMin != null ? ` · 时长 ${manual.durationMin} min` : ""}
            {manual.startedAt ? ` · ${shortTime(manual.startedAt)}` : ""}
            {manual.localCoverage != null
              ? ` · 本地${manual.localScope === "combined" ? "合计（主链+子代理）" : "主链"} ${manual.localCredits ?? t.credits}，覆盖 ${pct(manual.localCoverage)}`
              : " · 本地无 usage，无法对账"}
            {manual.note ? `。备注：${manual.note}` : ""}
          </Callout>
        )}

        {sub && sub.count === 0 && sub.reason && sub.reason !== "no-dir" && (
          <Callout tone="warning" title={`子代理账未汇总（${sub.reason}）`}>
            探测过的候选目录：{(sub.probedPaths ?? []).join("  |  ") || "（无）"}
          </Callout>
        )}

        {proxyAlarm && (
          <Callout tone="warning" title={`代理自动启动失败（${proxyAlarm.error}）`}>
            {proxyAlarm.error === "EADDRINUSE"
              ? `端口 ${proxyAlarm.port ?? "—"} 被占用，自定义模型将无法对话。请换一个空闲端口重启代理：${CLI} --setup-proxy --port <新端口>，并把模型 Base URL 改成新端口。`
              : `代理未就绪（${proxyAlarm.error}），自定义模型可能无法对话。请运行 ${CLI} --check-proxy 查看链路状态。`}
          </Callout>
        )}

        {r.proxy?.dormant && !proxyAlarm && (
          <Callout tone="info" title="代理长期空闲">
            代理已配置但超过 14 天没有记录到流量（可能你已改回官方模型）。如不再使用自定义模型，可运行{" "}
            {CLI} --stop-proxy 停用代理；保留也不影响官方模型。
          </Callout>
        )}

        {cwIsFallback && (
          <Callout tone="warning" title="上下文窗口为回退值 200K（未从 runtime-config / usage 反推 / ManualTruth 拿到真值）">
            本会话所有以窗口为分母的占比与「峰值建议」都可能偏大，而绝对 token 数（已改以 S 真值为底）不受影响。
            建议在 {"`.qoder-credits/overrides/<sessionId>.json`"} 里手工填 {"`contextWindow`"}（如 qwen3-max=1000000），或等一笔带 input_tokens+ratio 的往返写入转录后自动反推生效。
          </Callout>
        )}

        {hasUsage && netAdv && c.userContextLimit != null && (
          <Stack gap="component">
            <Callout
              tone={netAdv.tone}
              title={`你真实的上下文占用 ${pct(c.netUserRatio ?? c.peakUserRatio ?? 0)}（${human(curCtx)} / 阈值 ${human(c.userContextLimit)}・${limitSrcLabel}）`}
            >
              {netAdv.text}
              {limitIsDefault && (
                <Text tone="secondary">
                  {` ⚙ 这里的 ${human(c.userContextLimit)} 是插件内置默认值，不是你在 Qoder「模型管理」里设的真实上限（插件读不到那个设置）。想按真实阈值算：在 ~/.qoder-credits-proxy/config.json 填 "userContextLimit": <你的上限>（对所有会话生效），或对本会话在 .qoder-credits/overrides/<会话id>.json 填同名字段，重跑报告即生效。`}
                </Text>
              )}
            </Callout>
            {ctxRows.length > 0 && (
              <Table
                headers={["对比口径", "数值", "说明"]}
                rows={ctxRows}
                density="compact"
              />
            )}
          </Stack>
        )}

        {c.thresholdNotEnforced && (
          <Callout tone="warning" title="⚠ 自动压缩不会在你设的阈值触发（本条最重要）">
            你的模型物理窗口是 {cwText}，Qoder 的自动压缩要等上下文涨到窗口 ~85%（≈{human(Math.round(c.contextWindow * 0.85))}）才触发；
            你在模型管理里设的 {human(c.userContextLimit)} 上限远在其下，永远不会触发自动压缩。
            请照上面「你真实的上下文占用」那条，到点自己手动压缩，别等它自动压。
          </Callout>
        )}

        {lh && (
          <Callout tone={lhTone} title="链路健康度">
            {lhText}
          </Callout>
        )}

        <Callout tone="info" title="度量口径">
          计费输入总量逐笔锁定真值（优先级：代理 promptTokens &gt; 转录 usage.input_tokens &gt; ratio{availTag(av, "contextRatio")} × {cwText}
          {cwIsFallback ? "，未拿到真窗口、全部 token 数字随之带 ≈" : ""}）；各类别/文件按块估算规模比例分摊
          {availTag(av, "categoryShare", "derived")}。{identityText}
          {cachedText ? ` ${cachedText}` : ""}
          {covPartial
            ? ` Credits 只来自 ${cov!.trips}/${cov!.roundTrips} 笔往返（占计费输入 ${pct(cov!.tokenShare)}），其余往返走代理实测、不经 Qoder 计费，故不要拿 Credits 去除以计费输入总量算单价。`
            : ""}
        </Callout>

        {byReq.length > 0 && (
          <ReportSection
            title="逐请求明细（每一次往返）"
            description={
              showCreditsChart
                ? "每个 round-trip 的真实输入规模与 credits；曲线骤降处为上下文压缩重置（顶部四项为会话累计，此处为每一次）"
                : `每个 round-trip 的真实输入规模；曲线骤降处为上下文压缩重置。credits 曲线未画：本会话只有 ${cov?.trips ?? 0}/${cov?.roundTrips ?? byReq.length} 笔往返带 credits（其余走代理实测、不经 Qoder 计费），画出来是一地零。`
            }
            meta={`${byReq.length} 次往返 · 压缩 ${orDash(s.compactions, av, "compactions")} 次`}
            divided
          >
            {showCreditsChart ? (
              <ChartComparisonGrid>
                <ChartContainer title="每次输入 tokens" ariaLabel="每次输入 tokens">
                  <LineChart
                    categories={reqLabels}
                    series={[{ name: "输入 tokens", data: reqInputs, tone: "info" }]}
                    height={220}
                    valueFormatter={human}
                    ariaLabel="每次请求输入 tokens"
                  />
                </ChartContainer>
                <ChartContainer title="每次 credits" ariaLabel="每次 credits">
                  <LineChart
                    categories={reqLabels}
                    series={[{ name: "credits", data: reqCredits, tone: "warning" }]}
                    height={220}
                    ariaLabel="每次请求 credits"
                  />
                </ChartContainer>
              </ChartComparisonGrid>
            ) : (
              <ChartContainer title="每次输入 tokens" ariaLabel="每次输入 tokens">
                <LineChart
                  categories={reqLabels}
                  series={[{ name: "输入 tokens", data: reqInputs, tone: "info" }]}
                  height={220}
                  valueFormatter={human}
                  ariaLabel="每次请求输入 tokens"
                />
              </ChartContainer>
            )}
          </ReportSection>
        )}

        {hasUsage && ccItems.length > 0 && (
          <ReportSection
            title="压缩单笔成本（会话里最贵的那几笔调用）"
            description="每次压缩本身就是一笔普通模型调用：整份上下文当 prompt 进去、摘要当 completion 出来。此前报告只显示「压缩 N 次」，把单笔最贵的开销藏成了一个计数。「省下」= 压缩前规模 − 压缩后首轮实测输入（地板）。口径列：实测=能在代理日志里唯一对上的供应商真值；自估=客户端在压缩边界里写的前后规模。"
            meta={`${ccItems.length} 次 · 输入累计 ${human(ccost?.totals.preTokens ?? 0)} · 摘要累计 ${human(ccost?.totals.postTokens ?? 0)} · 累计省下 ${human(ccost?.totals.savedTokens ?? 0)} · 其中 ${ccost?.totals.measured ?? 0} 笔为供应商实测`}
            divided
          >
            <Table
              headers={["第几次", "时刻", "触发", "输入", "摘要输出", "压缩后首轮", "省下", "耗时", "口径"]}
              rows={compactRows}
              density="compact"
              stickyHeader
            />
          </ReportSection>
        )}

        {sub && subItems.length > 0 && (
          <ReportSection
            title="子代理账（Agent 派发）"
            description="子代理的每次往返只写进它自己的独立转录，不计入上方主链任何数字；官方 UI 的一场会话扣费 = 主链 + 各子代理。"
            meta={`${subItems.length} 个子代理 · 主链 ${t.credits} + 子代理 ${sub.totals.credits} = 合计 ${sub.combined.credits} Credits`}
            divided
          >
            <Table
              headers={["子代理", "类型", "往返", "计费输入", "峰值占比", "Credits", "原价", "备注"]}
              rows={subRows}
              rowTone={subTones}
              density="compact"
              stickyHeader
            />
          </ReportSection>
        )}

        {hasUsage && r.byCategory.length > 0 && (
          <ReportSection
            title="按类别占比"
            description={`会话计费输入 token 在各来源间的分布（完整划分，占比之和 = 100%）${sysNote}`}
            meta={human(t.billedInputTokens) + ` tokens${availTag(av, "categoryShare", "derived")}`}
            divided
          >
            <ChartContainer ariaLabel="按类别占比">
              <PieChart donut data={pieData} centerLabel="计费输入" valueFormatter={human} />
            </ChartContainer>
          </ReportSection>
        )}

        {hasUsage && catDetail.length > 0 && (
          <ReportSection
            title="按类别下钻（工具 / 文件路径）"
            description="每个类别的消耗再拆到工具与具体文件/路径：工具返回、工具调用可精确到路径，其余类别为整体（无文件归属）。数值均为重发计费 token。"
            meta={`占总额=占计费输入总量 · 占本类=占该类别 · 程数=存活往返累计${availTag(av, "fileShare", "derived")}`}
            divided
          >
            <Table
              headers={["类别", "工具", "文件 / 路径", "重发 tokens", "占总额", "占本类", "程数"]}
              rows={detailRows}
              rowTone={detailTones}
              density="compact"
              stickyHeader
            />
          </ReportSection>
        )}

        {hasUsage && tools.length > 0 && (
          <ReportSection
            title="按工具占比"
            description="各工具相关内容（调用参数 + 返回）重发累计的计费 token 占比（仅工具相关块，非完整划分，占比之和 < 100%）。「单次」= 平均每次调用带来的重发 token：总量榜会把「次数少但每次极贵」的工具埋掉，这一列专门把它捞出来。"
            meta={`token 占比${availTag(av, "toolShare", "derived")} · 调用次数为实测`}
            divided
          >
            <Stack gap="component">
              <ChartContainer ariaLabel="按工具占比">
                <BarChart horizontal categories={toolCategories} series={toolSeries} valueFormatter={human} ariaLabel="按工具占比" />
              </ChartContainer>
              <Table
                headers={["工具", "重发 tokens", "占比", "调用", "单次"]}
                rows={toolRows}
                density="compact"
                stickyHeader
              />
            </Stack>
          </ReportSection>
        )}

        {hasUsage && fileRows.length > 0 && (
          <ReportSection
            title="按文件占比"
            description="精确到路径/文件名：内容随上下文被重复发送累计的计费 token 占比（Top 20，仅可归因文件的块）。「单次读取」= 这个文件平均每次被读取最终烧掉多少（含此后每一程的重发）——读一次就烧掉十几万的文件，在按总量排序的榜上毫不起眼。"
            meta={`程数=存活往返累计 · 读取=返回次数（实测）${availTag(av, "fileShare", "derived")}`}
            divided
          >
            <Table
              headers={["文件", "种类", "重发 tokens", "占比", "单次读取", "程数", "读取"]}
              rows={fileRows}
              density="compact"
              stickyHeader
            />
          </ReportSection>
        )}

        {!hasUsage && residueRows.length > 0 && (
          <ReportSection
            title="工具调用与文件读取（计数为实测）"
            description="占比与 token 一律不可用，但 tool_use 块两种转录都写，所以「谁被调了几次、谁被读了几次」仍是真值 —— IDE 端不是一无所有。"
            meta={`${t.toolCalls ?? 0} 次工具调用${availTag(av, "toolCalls")} · ${t.fileReads ?? 0} 次文件读取${availTag(av, "fileReads")} · ${s.turns} 轮对话`}
            divided
          >
            <Table
              headers={["工具 / 文件", "种类", "调用", "读取", "程数"]}
              rows={residueRows}
              density="compact"
              stickyHeader
            />
          </ReportSection>
        )}
      </Stack>
    </ReportShell>
  );
}
