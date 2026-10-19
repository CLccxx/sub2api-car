type CarpoolForm = {
  carpool_members: number
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
  if (form.weekly_limit_usd == null || form.weekly_limit_usd === 0) {
    form.weekly_limit_usd = Math.floor(160000 / members) / 100
  }
}
