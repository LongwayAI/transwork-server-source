import { useQuery } from '@tanstack/react-query'
import { getUsersSubscriptionSummary } from '@/features/subscriptions/api'
import type { SubscriptionSummary } from '@/features/subscriptions/types'

// Gressio overlay (default theme) — fetches the current page's subscription
// summary for the admin user table. Kept here so the upstream `users-table.tsx`
// holds only a thin hook call (repo Rule 4, overlay-first). Non-blocking: any
// failure yields an empty map and the subscription columns render empty-state
// cells. placeholderData keeps the previous page's data on screen while the
// next page's summary loads.
export function useUsersSubscriptionSummary(
  userIds: number[]
): Record<number, SubscriptionSummary> {
  const { data = {} } = useQuery({
    queryKey: ['users-subscription-summary', userIds],
    queryFn: async () => {
      const result = await getUsersSubscriptionSummary(userIds)
      if (!result.success) {
        return {}
      }
      return (result.data || {}) as Record<number, SubscriptionSummary>
    },
    enabled: userIds.length > 0,
    placeholderData: (previousData) => previousData,
  })
  return data
}
