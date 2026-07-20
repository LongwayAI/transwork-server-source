import React from 'react';

// Gressio overlay (classic theme) — extra user-table columns (display name + email).
// Kept here so the upstream UsersColumnDefs.jsx holds only a thin import+spread
// seam (repo Rule 4, overlay-first). Both fields ride along on the user record the
// admin /api/user list already returns; email originates from the OIDC (Logto)
// UserInfo response captured at account creation.

const renderText = (text) => text || <span className='text-gray-400'>—</span>;

// getUserContactColumns returns the two display columns to be spread into the
// upstream users-table column list, right after the username column.
export function getUserContactColumns(t) {
  return [
    {
      title: t('显示名称'),
      dataIndex: 'display_name',
      render: (text) => renderText(text),
    },
    {
      title: t('邮箱'),
      dataIndex: 'email',
      render: (text) => renderText(text),
    },
  ];
}
