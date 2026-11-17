export default {
  title: 'Carpool service quota trades',
  description: 'Within the same carpool group only. Amounts refer to quota for sale. Buying quota does not extend your subscription expiry. Purchased quota expires when the account quota resets.',
  amount: 'Quota to list',
  amountPlaceholder: 'Enter the quota amount to list',
  serviceQuota: 'service quota',
  salePrice: 'Price',
  availableRule: 'The listed amount must be greater than 0 and strictly less than your available quota.',
  create: 'List quota',
  suggestedPrice: 'Current price per $ of quota = 1400 / ({estimate} × 4) = {price}',
  suggestedPriceUnavailable: 'Current price per $ of quota: account total estimate unavailable',
  reserve: 'Reserve',
  confirm: 'Confirm receipt and transfer quota',
  withdraw: 'Withdraw listing',
  mine: 'Me',
  seller: 'Seller',
  buyer: 'Buyer',
  createdAt: 'Listed at',
  reservedAt: 'Reserved at',
  completedAt: 'Completed at',
  empty: 'No available group listings or personal trade history',
  tabs: {
    label: 'Quota trade lists',
    current: 'Current listings',
    history: 'Trade history',
    currentEmpty: 'No current listings',
    historyEmpty: 'No trade history'
  },
  disabled: 'Quota trading is currently unavailable for this subscription. Existing records remain visible.',
  rules: 'How it works: reserve quota → contact and pay the seller in the WeChat group → seller confirms receipt → quota arrives. Contact an administrator to cancel a reservation.',
  paymentReceived: 'I have actually received the offline payment for this trade and authorize the quota transfer.',
  success: 'Trade action succeeded',
  failed: 'Trade request failed. Refresh and try again.',
  quota: {
    base_limit_usd: 'Dynamic base quota',
    adjustment_usd: 'Bought / sold adjustment',
    frozen_usd: 'Frozen by listings',
    used_usd: 'Used quota',
    available_usd: 'Available quota'
  },
  terms: {
    notice: 'Listing immediately freezes the service quota, even if the base quota changes. A natural account reset or reset-card reset ends this cycle’s listings and trade effects.'
  },
  status: {
    open: 'Open',
    reserved: 'Awaiting confirmation',
    completed: 'Completed',
    withdrawn: 'Withdrawn',
    expired: 'Expired (account cycle ended)'
  },
  createConfirmation: {
    title: 'Confirm quota listing',
    message: 'List {amount}$ of quota? Confirmation immediately freezes this amount. A natural account reset or reset-card reset ends this cycle’s listings and trade effects.',
    amount: 'Quota for sale',
    unitPrice: 'Current price per $ of quota',
    totalPrice: 'Suggested sale price',
    salePrice: 'Listing price (¥, required)',
    salePricePlaceholder: 'Enter the total price for this listing',
    invalidSalePrice: 'Price must be positive, have at most two decimal places, and not exceed 10 billion.',
    priceHint: 'The suggested price is for reference only. Enter your total listing price manually; payment remains offline in the WeChat group.'
  },
  messages: {
    reserve: 'Reserve {amount}$ of quota from listing #{id}? Arrange offline payment with the seller. Quota arrives after the seller confirms receipt. Purchased quota is valid only for the current cycle and expires when the account quota resets. Please use it promptly.',
    confirm: 'Have you received the offline payment for listing #{id} and wish to transfer {amount} USD service quota to the buyer? This completes the trade. Verify receipt first.',
    withdraw: 'Withdraw listing #{id} and release {amount} USD frozen service quota? Only open listings can be withdrawn.'
  },
  admin: {
    title: 'Carpool Quota Trades',
    description: 'View all current and historical trades. Administrators may cancel a reservation to reopen the listing (keeping quota frozen), confirm after verifying receipt, or withdraw an open listing.',
    status: 'Status',
    allStatuses: 'All statuses',
    group_id: 'Carpool group ID',
    account_id: 'Account ID',
    user_id: 'User ID (buyer or seller)',
    filter: 'Filter',
    invalidId: 'Filter IDs must be positive integers',
    order: 'Trade ID',
    group: 'Carpool group / account cycle',
    cycle: 'Cycle',
    actions: 'Actions',
    events: 'Event log',
    eventsTitle: 'Events for trade #{id}',
    noEvents: 'No events yet',
    adminActor: 'Administrator',
    systemActor: 'System',
    confirm: 'Confirm receipt on behalf',
    cancelReservation: 'Cancel reservation',
    actionTitle: 'Confirm administrator trade action',
    paymentVerified: 'I have verified that the seller actually received this offline payment and authorize completing the trade on their behalf.',
    messages: {
      confirm: 'Verify again: the seller has actually received the offline payment for listing #{id}. Confirming on their behalf completes the trade and transfers {amount} USD service quota. This does not process a payment or refund.',
      'cancel-reservation': 'Cancel the reservation for listing #{id}? It will reopen with {amount} USD service quota still frozen. No refund is processed. To withdraw it, reopen it first and then withdraw separately.',
      withdraw: 'Withdraw open listing #{id} and release {amount} USD frozen service quota?'
    }
  },
  eventActions: {
    create: 'Listed and froze quota',
    reserve: 'Reserved',
    confirm: 'Confirmed receipt and completed',
    cancel_reservation: 'Cancelled reservation and reopened',
    withdraw: 'Withdrew and released frozen quota',
    expire: 'Expired at account cycle end',
    expired: 'Expired at account cycle end'
  }
}
