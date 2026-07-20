/*
 * Gressio overlay (default theme) — extra user-table columns (display name + email).
 *
 * Kept here so the upstream `users-columns.tsx` holds only a thin import+spread
 * seam (repo Rule 4, overlay-first). Both fields ride along on the user record the
 * admin `/api/user` list already returns; email originates from the OIDC (Logto)
 * UserInfo response captured at account creation.
 */
import { type ColumnDef } from '@tanstack/react-table'
import { type TFunction } from 'i18next'
import { DataTableColumnHeader } from '@/components/data-table'
import { LongText } from '@/components/long-text'
import { type User } from '@/features/users/types'

// getUserContactColumns returns the two display columns to be spread into the
// upstream users-table column list, right after the username column.
export function getUserContactColumns(t: TFunction): ColumnDef<User>[] {
  return [
    {
      accessorKey: 'display_name',
      header: ({ column }) => (
        <DataTableColumnHeader column={column} title={t('Display Name')} />
      ),
      cell: ({ row }) => {
        const displayName = row.getValue('display_name') as string
        return displayName ? (
          <LongText className='max-w-[160px]'>{displayName}</LongText>
        ) : (
          <span className='text-muted-foreground/50 text-xs'>—</span>
        )
      },
      enableSorting: false,
      meta: { label: t('Display Name'), mobileHidden: true },
    },
    {
      accessorKey: 'email',
      header: ({ column }) => (
        <DataTableColumnHeader column={column} title={t('Email')} />
      ),
      cell: ({ row }) => {
        const email = row.getValue('email') as string | undefined
        return email ? (
          <LongText className='max-w-[200px]'>{email}</LongText>
        ) : (
          <span className='text-muted-foreground/50 text-xs'>—</span>
        )
      },
      enableSorting: false,
      meta: { label: t('Email'), mobileHidden: true },
    },
  ]
}
