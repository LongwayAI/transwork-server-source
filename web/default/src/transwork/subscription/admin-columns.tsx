/*
 * Gressio overlay (default theme) — Stripe subscription admin columns.
 *
 * All merge/column/cancel logic for the F2 admin view lives here so the upstream
 * `user-subscriptions-dialog.tsx` keeps a thin import+render seam only (repo
 * Rule 4, plan §0.1 overlay-first). The overlay admin endpoints live under
 * `transwork/handler/` and are reached through `/api/transwork/subscription/...`.
 */
import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { api } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { TableCell, TableHead } from '@/components/ui/table'
import { ConfirmDialog } from '@/components/confirm-dialog'
import { formatTimestamp } from '@/features/subscriptions/lib'

// Mirrors the overlay Go model `StripeSubscriptionLink` (transwork/model). Only
// the display fields the admin view needs are typed here.
export interface StripeSubscriptionLink {
  id: number
  stripe_subscription_id: string
  user_subscription_id: number
  status: string
  auto_renew: boolean
  current_period_end: number
}

// buildLinkMap joins Stripe link rows to the dialog's subscription rows by the
// local `user_subscription_id` (design B3/O2). Pure + unit-testable (WS-1.8 RED).
export function buildLinkMap(
  links: StripeSubscriptionLink[]
): Map<number, StripeSubscriptionLink> {
  const map = new Map<number, StripeSubscriptionLink>()
  ;(links || []).forEach((link) => {
    if (link && link.user_subscription_id) {
      map.set(link.user_subscription_id, link)
    }
  })
  return map
}

// useAdminSubscriptionLinks fetches the admin Stripe links for a user and
// exposes the merge map plus a refetch (used after a cancel). Best-effort: on
// failure the dialog still renders core subscription data.
export function useAdminSubscriptionLinks(
  userId: number | undefined,
  open: boolean
) {
  const [linkMap, setLinkMap] = useState<Map<number, StripeSubscriptionLink>>(
    () => new Map()
  )
  const [reloadKey, setReloadKey] = useState(0)

  useEffect(() => {
    let cancelled = false
    if (!open || !userId) {
      setLinkMap(new Map())
      return
    }
    api
      .get(`/api/transwork/subscription/admin/links?user_id=${userId}`)
      .then((res) => {
        if (!cancelled && res.data?.success) {
          setLinkMap(buildLinkMap(res.data.data || []))
        }
      })
      .catch(() => {})
    return () => {
      cancelled = true
    }
  }, [userId, open, reloadKey])

  return { linkMap, refetch: () => setReloadKey((k) => k + 1) }
}

// AdminSubscriptionLinkHeadCells — the three extra <th> for the dialog header.
export function AdminSubscriptionLinkHeadCells() {
  const { t } = useTranslation()
  return (
    <>
      <TableHead>{t('Auto-renew')}</TableHead>
      <TableHead>{t('Stripe subscription')}</TableHead>
      <TableHead>{t('Current period end')}</TableHead>
    </>
  )
}

// AdminSubscriptionLinkBodyCells — the three extra <td> for a subscription row.
export function AdminSubscriptionLinkBodyCells(props: {
  link?: StripeSubscriptionLink
}) {
  const { t } = useTranslation()
  const { link } = props
  return (
    <>
      <TableCell>
        {link
          ? link.auto_renew && link.status !== 'canceled'
            ? t('Yes')
            : t('No')
          : '-'}
      </TableCell>
      <TableCell>
        {link?.stripe_subscription_id ? (
          <span
            className='block max-w-[140px] truncate font-mono text-xs'
            title={link.stripe_subscription_id}
          >
            {link.stripe_subscription_id}
          </span>
        ) : (
          '-'
        )}
      </TableCell>
      <TableCell className='text-xs'>
        {formatTimestamp(link?.current_period_end ?? 0)}
      </TableCell>
    </>
  )
}

// CancelStripeButton — row action that cancels the live Stripe subscription
// (endpoint B7). Shown only when there is an auto-renewing Stripe link to cancel.
export function CancelStripeButton(props: {
  link?: StripeSubscriptionLink
  onCanceled?: () => void
}) {
  const { t } = useTranslation()
  const { link, onCanceled } = props
  const [open, setOpen] = useState(false)
  const [pending, setPending] = useState(false)

  // Only offer cancel while the link is still live (active/past_due) and
  // auto-renewing — a canceled link has nothing left to cancel.
  if (
    !link ||
    !link.stripe_subscription_id ||
    !link.auto_renew ||
    link.status === 'canceled'
  )
    return null

  const handleConfirm = async () => {
    setPending(true)
    try {
      const res = await api.post(
        '/api/transwork/subscription/admin/cancel-stripe',
        { stripe_subscription_id: link.stripe_subscription_id }
      )
      if (res.data?.success) {
        toast.success(t('Canceled'))
        onCanceled?.()
        setOpen(false)
      }
    } catch {
      toast.error(t('Request failed'))
    } finally {
      setPending(false)
    }
  }

  return (
    <>
      <Button
        size='sm'
        variant='destructive'
        disabled={pending}
        onClick={() => setOpen(true)}
      >
        {t('Cancel subscription')}
      </Button>
      {open && (
        <ConfirmDialog
          open
          onOpenChange={(v) => !v && setOpen(false)}
          title={t('Cancel subscription')}
          desc={t(
            'After canceling, auto-renewal will stop at the end of the current period. Continue?'
          )}
          handleConfirm={handleConfirm}
          isLoading={pending}
          destructive
        />
      )}
    </>
  )
}
