import type { CurrencyFormatOptions } from '@/lib/currency'

export const CREDITS_PER_USD = 100
export const TRANSWORK_CREDIT_LABEL = 'Credits'

export type CreditFormatOptions = CurrencyFormatOptions & {
  rounding?: 'floor' | 'exact'
}

function removeTrailingZeros(value: string): string {
  if (!value.includes('.')) return value
  return value.replace(/(\.[0-9]*?)0+$/, '$1').replace(/\.$/, '')
}

export function formatCreditNumber(
  value: number,
  options?: CreditFormatOptions
): string {
  const abs = Math.abs(value)
  const abbreviate = options?.abbreviate ?? false

  if (abbreviate && abs >= 1_000_000) {
    return `${removeTrailingZeros((value / 1_000_000).toFixed(1))}M`
  }

  if (abbreviate && abs >= 100_000) {
    return `${removeTrailingZeros((value / 1_000).toFixed(1))}k`
  }

  const digits =
    abs >= 1 ? (options?.digitsLarge ?? 0) : (options?.digitsSmall ?? 2)
  return new Intl.NumberFormat(undefined, {
    minimumFractionDigits: 0,
    maximumFractionDigits: digits,
  }).format(value)
}

export function quotaToTransworkCreditsWithUnit(
  quota: number | null | undefined,
  quotaPerUnit: number
): number {
  if (quota == null || Number.isNaN(quota)) return 0

  const safeQuotaPerUnit = quotaPerUnit > 0 ? quotaPerUnit : 500_000
  return (quota / safeQuotaPerUnit) * CREDITS_PER_USD
}

export function formatTransworkCreditsWithUnit(
  quota: number | null | undefined,
  quotaPerUnit: number,
  options?: CreditFormatOptions
): string {
  if (quota == null || Number.isNaN(quota)) return '-'

  const rawCredits = quotaToTransworkCreditsWithUnit(quota, quotaPerUnit)
  const credits =
    options?.rounding === 'exact' ? rawCredits : Math.floor(rawCredits)

  return `${formatCreditNumber(credits, options)} credits`
}

export function parseTransworkCreditsToQuotaWithUnit(
  credits: number,
  quotaPerUnit: number
): number {
  if (!Number.isFinite(credits)) return 0

  const safeQuotaPerUnit = quotaPerUnit > 0 ? quotaPerUnit : 500_000
  return Math.round((credits / CREDITS_PER_USD) * safeQuotaPerUnit)
}
