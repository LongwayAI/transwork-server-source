import { useMemo, useState } from 'react'
import {
  type SortingState,
  type VisibilityState,
  getCoreRowModel,
  getPaginationRowModel,
  getSortedRowModel,
  useReactTable,
} from '@tanstack/react-table'
import { RefreshCw } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { cn } from '@/lib/utils'
import { Button } from '@/components/ui/button'
import { DataTablePage } from '@/components/data-table'
import { SectionPageLayout } from '@/components/layout'
import { useWaitlistSubmissions } from './api'
import { useWaitlistColumns } from './waitlist-columns'

export function WaitlistPage() {
  const { t } = useTranslation()
  const columns = useWaitlistColumns()
  const [sorting, setSorting] = useState<SortingState>([])
  const [columnVisibility, setColumnVisibility] = useState<VisibilityState>({})

  const { data, isLoading, isFetching, refetch } = useWaitlistSubmissions()

  const submissions = useMemo(() => data || [], [data])

  const table = useReactTable({
    data: submissions,
    columns,
    state: { sorting, columnVisibility },
    onSortingChange: setSorting,
    onColumnVisibilityChange: setColumnVisibility,
    getCoreRowModel: getCoreRowModel(),
    getPaginationRowModel: getPaginationRowModel(),
    getSortedRowModel: getSortedRowModel(),
  })

  return (
    <SectionPageLayout>
      <SectionPageLayout.Title>{t('Waitlist')}</SectionPageLayout.Title>
      <SectionPageLayout.Description>
        {t('Waitlist submissions from prospective users')}
      </SectionPageLayout.Description>
      <SectionPageLayout.Actions>
        <Button
          size='sm'
          variant='outline'
          onClick={() => refetch()}
          disabled={isFetching}
        >
          <RefreshCw className={cn('h-4 w-4', isFetching && 'animate-spin')} />
          {t('Refresh')}
        </Button>
      </SectionPageLayout.Actions>
      <SectionPageLayout.Content>
        <DataTablePage
          table={table}
          columns={columns}
          isLoading={isLoading}
          isFetching={isFetching}
          emptyTitle={t('No waitlist submissions yet')}
          skeletonKeyPrefix='waitlist-skeleton'
        />
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
