/*
 * Gressio overlay (default theme) — subscription columns (plan / monthly credit /
 * status / next reset) for the admin user table.
 *
 * Kept here so the upstream `users-columns.tsx` holds only a thin import+spread
 * seam (repo Rule 4, overlay-first). `subscriptionSummary` maps user id -> that
 * user's primary in-period subscription summary, fetched by the overlay
 * `useUsersSubscriptionSummary` hook; a missing entry renders an em-dash cell.
 */
import { type ColumnDef } from '@tanstack/react-table'
import { type TFunction } from 'i18next'
import { formatQuota, formatTimestamp } from '@/lib/format'
import { DataTableColumnHeader } from '@/components/data-table'
import { StatusBadge } from '@/components/status-badge'
import { type SubscriptionSummary } from '@/features/subscriptions/types'
import { type User } from '@/features/users/types'

// getUserSubscriptionColumns returns the four subscription columns to be spread
// into the upstream users-table column list, after the quota column.
export function getUserSubscriptionColumns(
  t: TFunction,
  subscriptionSummary: Record<number, SubscriptionSummary> = {}
): ColumnDef<User>[] {
  return [
    {
      id: 'subscription_plan',
      header: ({ column }) => (
        <DataTableColumnHeader column={column} title={t('Plan')} />
      ),
      cell: ({ row }) => {
        const sub = subscriptionSummary[row.original.id]
        if (!sub) {
          return <span className='text-muted-foreground/50 text-xs'>—</span>
        }
        return (
          <span className='text-sm'>
            {sub.plan_title || t('Unknown plan')}
          </span>
        )
      },
      enableSorting: false,
      meta: { label: t('Plan') },
    },
    {
      id: 'subscription_quota',
      header: ({ column }) => (
        <DataTableColumnHeader column={column} title={t('Monthly credit')} />
      ),
      cell: ({ row }) => {
        const sub = subscriptionSummary[row.original.id]
        if (!sub) {
          return <span className='text-muted-foreground/50 text-xs'>—</span>
        }
        const remaining = sub.amount_total - sub.amount_used
        return (
          <span className='text-xs tabular-nums'>
            {formatQuota(remaining)} / {formatQuota(sub.amount_total)}
          </span>
        )
      },
      enableSorting: false,
      meta: { label: t('Monthly credit') },
    },
    {
      id: 'subscription_status',
      header: ({ column }) => (
        <DataTableColumnHeader column={column} title={t('Sub status')} />
      ),
      cell: ({ row }) => {
        const sub = subscriptionSummary[row.original.id]
        if (!sub) {
          return <span className='text-muted-foreground/50 text-xs'>—</span>
        }
        const variant = sub.status === 'active' ? 'success' : 'warning'
        return (
          <StatusBadge label={sub.status} variant={variant} copyable={false} />
        )
      },
      enableSorting: false,
      meta: { label: t('Sub status') },
    },
    {
      id: 'subscription_reset',
      header: ({ column }) => (
        <DataTableColumnHeader column={column} title={t('Next reset')} />
      ),
      cell: ({ row }) => {
        const sub = subscriptionSummary[row.original.id]
        if (!sub || !sub.next_reset_time) {
          return <span className='text-muted-foreground/50 text-xs'>—</span>
        }
        return (
          <span className='text-muted-foreground text-sm'>
            {formatTimestamp(sub.next_reset_time)}
          </span>
        )
      },
      enableSorting: false,
      meta: { label: t('Next reset'), mobileHidden: true },
    },
  ]
}
