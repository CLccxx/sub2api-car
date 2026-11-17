export default {
  title: '拼车服务额度交易',
  description: '仅限同一拼车组。金额为售出额度，购买额度不会延长个人订阅有效期。购买额度重置之后失效。',
  amount: '挂出额度',
  amountPlaceholder: '请输出要挂出的额度',
  serviceQuota: '服务额度',
  salePrice: '售价',
  availableRule: '挂出额度必须大于 0 且严格小于当前可用额度。',
  create: '挂出额度',
  suggestedPrice: '当前每$额度单价 = 1400 / ({estimate} × 4) = {price}',
  suggestedPriceUnavailable: '当前每$额度单价：暂无账号预估总额度',
  reserve: '订购',
  confirm: '确认已收款并转移额度',
  withdraw: '撤下挂单',
  mine: '我的',
  seller: '卖方',
  buyer: '买方',
  createdAt: '挂出时间',
  reservedAt: '订购时间',
  completedAt: '成交时间',
  empty: '暂无本组可订购挂单或我的交易历史',
  tabs: {
    label: '额度交易列表',
    current: '当前挂单',
    history: '交易记录',
    currentEmpty: '暂无当前有效挂单',
    historyEmpty: '暂无交易记录'
  },
  disabled: '当前订阅暂不支持额度交易，已有记录仍保留展示。',
  rules: '交易流程：订购额度 → 微信群联系卖方并转账 → 卖方确认收款 → 额度到账。如需取消订购，请联系管理员。',
  paymentReceived: '我已实际收到本笔交易的线下款项，确认转移额度。',
  success: '交易操作成功',
  failed: '交易请求失败，请刷新后重试',
  quota: {
    base_limit_usd: '动态基础额度',
    adjustment_usd: '买入／卖出调整',
    frozen_usd: '挂单冻结',
    used_usd: '已用额度',
    available_usd: '当前可用额度'
  },
  terms: {
    notice: '挂出即立即冻结相应服务额度，基础额度变化不会解除冻结。账号自然重置或卡重置时，本期挂单及交易效果结束。'
  },
  status: {
    open: '待订购',
    reserved: '待确认',
    completed: '已完成',
    withdrawn: '已撤下',
    expired: '已过期（账号周期结束）'
  },
  createConfirmation: {
    title: '确认挂出额度',
    message: '确认挂出 {amount}$ 额度？确认后将立即冻结相应额度。账号自然重置或使用重置卡后，本期挂单及交易效果结束。',
    amount: '出售额度',
    unitPrice: '当前每$额度单价',
    totalPrice: '建议出售价格',
    salePrice: '挂出售价（¥，必填）',
    salePricePlaceholder: '请输入本次挂单的总售价',
    invalidSalePrice: '售价必须大于 0，最多两位小数，且不超过 100 亿。',
    priceHint: '建议出售价格仅供参考，请手动填写本次挂单的总售价，付款仍在线下微信群完成。'
  },
  messages: {
    reserve: '确认订购挂单 #{id} 的 {amount}$ 额度？请线下与卖方商议支付，卖方确认收款后额度到账。订购的额度仅在本期有效，账号额度重置之后失效，请尽快使用。',
    confirm: '确认已收到挂单 #{id} 的线下款项并转移 {amount} USD 服务额度给买方？该操作将完成交易，请核实收款。',
    withdraw: '确认撤下挂单 #{id}？将释放冻结的 {amount} USD 服务额度。仅待订购挂单可撤下。'
  },
  admin: {
    title: '拼车额度交易',
    description: '查看所有当前及历史交易。仅管理员可取消待确认订购并恢复待订购状态（保留冻结），可核实收款后代确认，或撤下待订购挂单。',
    status: '状态',
    allStatuses: '全部状态',
    group_id: '拼车组 ID',
    account_id: '账号 ID',
    user_id: '用户 ID（买方或卖方）',
    filter: '筛选',
    invalidId: '筛选 ID 必须为正整数',
    order: '交易 ID',
    group: '拼车组／账号周期',
    cycle: '周期',
    actions: '操作',
    events: '事件日志',
    eventsTitle: '交易 #{id} 事件日志',
    noEvents: '暂无事件',
    adminActor: '管理员',
    systemActor: '系统',
    confirm: '代确认收款',
    cancelReservation: '取消订购',
    actionTitle: '确认管理员交易操作',
    paymentVerified: '我已核实卖方确实收到本笔线下款项，确认代为完成交易。',
    messages: {
      confirm: '请二次核实：挂单 #{id} 的线下款项确已由卖方收到。代确认将完成交易，转移 {amount} USD 服务额度；这不是支付或退款操作。',
      'cancel-reservation': '确认取消挂单 #{id} 的订购？挂单将恢复待订购，仍保留冻结的 {amount} USD 服务额度，不会触发退款；如需撤下请恢复待订购后另行操作。',
      withdraw: '确认撤下待订购挂单 #{id} 并释放冻结的 {amount} USD 服务额度？'
    }
  },
  eventActions: {
    create: '挂出并冻结额度',
    reserve: '订购',
    confirm: '确认收款并完成',
    cancel_reservation: '取消订购并恢复待订购',
    withdraw: '撤下并释放冻结',
    expire: '账号周期结束，交易过期',
    expired: '账号周期结束，交易过期'
  }
}
