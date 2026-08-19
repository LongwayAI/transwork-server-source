import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'

// ============================================================================
// Waitlist Admin API
// ============================================================================

/** A single waitlist submission row (newest-first from the backend). */
export interface WaitlistSubmission {
  id: number
  user_id: number
  email: string
  name: string
  job: string
  role: string
  use_case: string
  /** Unix timestamp in seconds. */
  created_at: number
}

interface WaitlistAdminResponse {
  success: boolean
  message?: string
  data: WaitlistSubmission[]
}

/** Fetch all waitlist submissions (admin-only, session-cookie auth). */
export async function getWaitlistSubmissions(): Promise<WaitlistSubmission[]> {
  const res = await api.get<WaitlistAdminResponse>(
    '/api/transwork/waitlist/admin'
  )
  return res.data.data || []
}

/** React Query hook wrapping {@link getWaitlistSubmissions}. */
export function useWaitlistSubmissions() {
  return useQuery({
    queryKey: ['admin-waitlist-submissions'],
    queryFn: getWaitlistSubmissions,
    placeholderData: (prev) => prev,
  })
}
