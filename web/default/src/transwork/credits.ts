import { getCurrencyDisplay } from '@/lib/currency'
import {
  CREDITS_PER_USD,
  formatTransworkCreditsWithUnit,
  parseTransworkCreditsToQuotaWithUnit,
  quotaToTransworkCreditsWithUnit,
  type CreditFormatOptions,
} from './credits-core'

export { CREDITS_PER_USD }

export function formatTransworkCredits(
  quota: number | null | undefined,
  options?: CreditFormatOptions
): string {
  if (quota == null || Number.isNaN(quota)) return '-'

  const { config } = getCurrencyDisplay()
  const quotaPerUnit = config.quotaPerUnit > 0 ? config.quotaPerUnit : 500_000
  return formatTransworkCreditsWithUnit(quota, quotaPerUnit, options)
}

export function parseTransworkCreditsToQuota(credits: number): number {
  if (!Number.isFinite(credits)) return 0

  const { config } = getCurrencyDisplay()
  const quotaPerUnit = config.quotaPerUnit > 0 ? config.quotaPerUnit : 500_000
  return parseTransworkCreditsToQuotaWithUnit(credits, quotaPerUnit)
}

export function quotaToTransworkCredits(
  quota: number | null | undefined
): number {
  if (quota == null || Number.isNaN(quota)) return 0

  const { config } = getCurrencyDisplay()
  const quotaPerUnit = config.quotaPerUnit > 0 ? config.quotaPerUnit : 500_000
  return quotaToTransworkCreditsWithUnit(quota, quotaPerUnit)
}
