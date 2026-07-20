/*
Copyright (C) 2025 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import React from 'react';
import { Tag } from '@douyinfe/semi-ui';
import { renderQuota, timestamp2string } from '../../helpers';

// Gressio overlay (classic theme) — subscription columns (plan / monthly credit /
// status / next reset) for the admin user table. Kept here so the upstream
// UsersColumnDefs.jsx holds only a thin import+spread seam (repo Rule 4,
// overlay-first). subscriptionSummary maps user id -> that user's primary
// in-period subscription summary, fetched by the overlay useSubscriptionSummary
// hook; a missing entry renders an em-dash placeholder.

// getUserSubscriptionColumns returns the four subscription columns to be spread
// into the upstream users-table column list, after the quota-usage column.
export function getUserSubscriptionColumns(t, subscriptionSummary = {}) {
  return [
    {
      title: t('订阅套餐'),
      key: 'subscription_plan',
      render: (text, record) => {
        const sub = subscriptionSummary[record.id];
        if (!sub) return <span className='text-gray-400'>—</span>;
        return (
          <Tag color='blue' shape='circle'>
            {sub.plan_title || t('未知套餐')}
          </Tag>
        );
      },
    },
    {
      title: t('订阅额度(月)'),
      key: 'subscription_quota',
      render: (text, record) => {
        const sub = subscriptionSummary[record.id];
        if (!sub) return <span className='text-gray-400'>—</span>;
        const total = parseInt(sub.amount_total) || 0;
        const remain = total - (parseInt(sub.amount_used) || 0);
        return (
          <span className='text-xs'>{`${renderQuota(remain)} / ${renderQuota(total)}`}</span>
        );
      },
    },
    {
      title: t('订阅状态'),
      key: 'subscription_status',
      render: (text, record) => {
        const sub = subscriptionSummary[record.id];
        if (!sub) return <span className='text-gray-400'>—</span>;
        const colorMap = { active: 'green', cancelled: 'orange', expired: 'grey' };
        return (
          <Tag color={colorMap[sub.status] || 'grey'} shape='circle'>
            {sub.status}
          </Tag>
        );
      },
    },
    {
      title: t('下次重置'),
      key: 'subscription_reset',
      render: (text, record) => {
        const sub = subscriptionSummary[record.id];
        if (!sub || !sub.next_reset_time) return <span className='text-gray-400'>—</span>;
        return (
          <span className='text-xs'>
            {timestamp2string(sub.next_reset_time).split(' ')[0]}
          </span>
        );
      },
    },
  ];
}
