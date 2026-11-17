export const DEFAULT_CARPOOL_BONUS_USD = 100
export const MAX_CARPOOL_BONUS_USD = 1e10

export function isValidCarpoolBonus(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value) &&
    value >= 0 && value <= MAX_CARPOOL_BONUS_USD &&
    // Compare the dollar value, not integer cents: e.g. 0.29 * 100 is not exactly 29.
    value === Math.round(value * 100) / 100
}

type CarpoolForm = {
  carpool_members: number
  carpool_bonus_usd?: number
  name: string
  description: string
  daily_limit_usd: number | null
  weekly_limit_usd: number | null
  monthly_limit_usd: number | null
}

export function applyCarpoolMembers(form: CarpoolForm, members: number) {
  if (members !== 0 && (!Number.isInteger(members) || members < 2 || members > 5)) return
  form.carpool_members = members
  if (!members) return
  form.name = `${members} 人车分组`
  form.description = `${members} 人均分额度`
  form.daily_limit_usd = null
  form.monthly_limit_usd = null
  if (form.weekly_limit_usd == null || form.weekly_limit_usd <= 0) {
    form.weekly_limit_usd = bootstrapWeeklyLimit(members, form.carpool_bonus_usd ?? DEFAULT_CARPOOL_BONUS_USD)
  }
}

function bootstrapWeeklyLimit(members: number, bonus: number) {
  // Match the backend's $1600 bootstrap estimate, dividing and adding in cents.
  return (Math.floor(160000 / members) + Math.round(bonus * 100)) / 100
}

export function applyCarpoolBonus(form: CarpoolForm, bonus: number) {
  if (!isValidCarpoolBonus(bonus)) return
  const oldBonus = form.carpool_bonus_usd ?? DEFAULT_CARPOOL_BONUS_USD
  if (bonus === oldBonus) return
  form.carpool_bonus_usd = bonus
  if (!form.carpool_members || form.weekly_limit_usd == null) return

  // Preview only: saving applies the delta to the latest cap on the server.
  // Zero is still an existing cap, so changing the bonus remains reversible.
  form.weekly_limit_usd = (Math.round(form.weekly_limit_usd * 100) -
    Math.round(oldBonus * 100) + Math.round(bonus * 100)) / 100
}
