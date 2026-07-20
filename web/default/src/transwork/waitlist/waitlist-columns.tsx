import { useMemo } from 'react'
import { type ColumnDef } from '@tanstack/react-table'
import { useTranslation } from 'react-i18next'
import { formatTimestamp } from '@/lib/format'
import { DataTableColumnHeader } from '@/components/data-table'
import { type WaitlistSubmission } from './api'

export function useWaitlistColumns(): ColumnDef<WaitlistSubmission>[] {
  const { t } = useTranslation()

  return useMemo(
    (): ColumnDef<WaitlistSubmission>[] => [
      {
        accessorKey: 'name',
        meta: { label: t('Name'), mobileTitle: true },
        header: ({ column }) => (
          <DataTableColumnHeader column={column} title={t('Name')} />
        ),
        cell: ({ row }) => (
          <span className='font-medium'>{row.original.name || '-'}</span>
        ),
      },
      {
        accessorKey: 'email',
        meta: { label: t('Email') },
        header: ({ column }) => (
          <DataTableColumnHeader column={column} title={t('Email')} />
        ),
        cell: ({ row }) => (
          <span className='text-muted-foreground'>
            {row.original.email || '-'}
          </span>
        ),
      },
      {
        accessorKey: 'job',
        meta: { label: t('Job') },
        header: ({ column }) => (
          <DataTableColumnHeader column={column} title={t('Job')} />
        ),
        cell: ({ row }) => (
          <span className='text-muted-foreground'>
            {row.original.job || '-'}
          </span>
        ),
      },
      {
        accessorKey: 'role',
        meta: { label: t('Role') },
        header: ({ column }) => (
          <DataTableColumnHeader column={column} title={t('Role')} />
        ),
        cell: ({ row }) => (
          <span className='text-muted-foreground'>
            {row.original.role || '-'}
          </span>
        ),
      },
      {
        accessorKey: 'use_case',
        meta: { label: t('Use Case'), mobileHidden: true },
        header: ({ column }) => (
          <DataTableColumnHeader column={column} title={t('Use Case')} />
        ),
        cell: ({ row }) => (
          <span className='text-muted-foreground line-clamp-2 max-w-[280px]'>
            {row.original.use_case || '-'}
          </span>
        ),
      },
      {
        accessorKey: 'created_at',
        meta: { label: t('Submitted'), mobileHidden: true },
        header: ({ column }) => (
          <DataTableColumnHeader column={column} title={t('Submitted')} />
        ),
        cell: ({ row }) => (
          <span className='text-muted-foreground text-sm'>
            {row.original.created_at
              ? formatTimestamp(row.original.created_at)
              : '-'}
          </span>
        ),
      },
      {
        accessorKey: 'user_id',
        meta: { label: t('User ID'), mobileHidden: true },
        header: ({ column }) => (
          <DataTableColumnHeader column={column} title={t('User ID')} />
        ),
        cell: ({ row }) => (
          <span className='text-muted-foreground'>
            {row.original.user_id ? `#${row.original.user_id}` : '-'}
          </span>
        ),
        size: 90,
      },
    ],
    [t]
  )
}
