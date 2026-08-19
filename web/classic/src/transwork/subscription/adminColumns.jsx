import React from 'react';
import { Button, Modal, Typography } from '@douyinfe/semi-ui';
import { API, showError, showSuccess } from '../../helpers';

// Gressio overlay (classic theme) — Stripe subscription admin columns.
// All merge/column/cancel logic for the F2 admin view lives here so the upstream
// UserSubscriptionsModal.jsx keeps a thin import+spread seam only (repo Rule 4,
// plan §0.1 overlay-first). The overlay admin endpoints live under
// transwork/handler/ and are reached through /api/transwork/subscription/...
const { Text } = Typography;

function formatTs(ts) {
  if (!ts) return '-';
  return new Date(ts * 1000).toLocaleString();
}

// buildLinkMap joins Stripe link rows to the modal's subscription rows by the
// local `user_subscription_id` (design B3/O2). Pure + unit-testable (WS-1.8 RED).
export function buildLinkMap(links) {
  const map = new Map();
  (links || []).forEach((link) => {
    if (link && link.user_subscription_id) {
      map.set(link.user_subscription_id, link);
    }
  });
  return map;
}

// loadLinkMap fetches the admin Stripe links for a user and returns the merge
// map. Best-effort: on any failure the modal still renders core subscription
// data, just without the Stripe columns.
export async function loadLinkMap(userId) {
  if (!userId) return new Map();
  try {
    const res = await API.get(
      `/api/transwork/subscription/admin/links?user_id=${userId}`,
    );
    if (res.data?.success) return buildLinkMap(res.data.data || []);
  } catch (e) {
    // swallow — non-fatal for the modal
  }
  return new Map();
}

// cancelStripe confirms, then cancels the live Stripe subscription (endpoint B7)
// and reloads the modal. Idempotent server-side.
function cancelStripe(link, t, reload) {
  Modal.confirm({
    title: t('取消订阅'),
    content: t('取消后将在本期结束时停止续订。是否继续？'),
    centered: true,
    okType: 'danger',
    onOk: async () => {
      try {
        const res = await API.post(
          '/api/transwork/subscription/admin/cancel-stripe',
          { stripe_subscription_id: link.stripe_subscription_id },
        );
        if (res.data?.success) {
          showSuccess(t('已取消'));
          reload && reload();
        } else {
          showError(res.data?.message || t('操作失败'));
        }
      } catch (e) {
        showError(t('请求失败'));
      }
    },
  });
}

// getAdminColumns returns the three Stripe display columns plus a cancel action,
// to be spread into the upstream modal's column list. Column shape matches the
// modal's existing `{ title, key, width, render }` defs.
export function getAdminColumns({ t, linkMap, reload }) {
  const linkOf = (record) => linkMap.get(record?.subscription?.id);
  return [
    {
      title: t('自动续订'),
      key: 'stripe_auto_renew',
      width: 100,
      render: (_, record) => {
        const link = linkOf(record);
        if (!link) return <Text type='tertiary'>-</Text>;
        return (
          <Text>
            {link.auto_renew && link.status !== 'canceled' ? t('是') : t('否')}
          </Text>
        );
      },
    },
    {
      title: t('Stripe 订阅号'),
      key: 'stripe_subscription_id',
      width: 160,
      render: (_, record) => {
        const link = linkOf(record);
        const id = link?.stripe_subscription_id;
        if (!id) return <Text type='tertiary'>-</Text>;
        return (
          <Text ellipsis={{ showTooltip: true }} style={{ maxWidth: 150 }}>
            {id}
          </Text>
        );
      },
    },
    {
      title: t('本期结束'),
      key: 'current_period_end',
      width: 180,
      render: (_, record) => {
        const link = linkOf(record);
        return (
          <Text type='secondary' className='text-xs'>
            {formatTs(link?.current_period_end)}
          </Text>
        );
      },
    },
    {
      title: '',
      key: 'stripe_operate',
      width: 120,
      render: (_, record) => {
        const link = linkOf(record);
        // Only offer cancel while the link is still live (active/past_due) and
        // auto-renewing — a canceled link has nothing left to cancel.
        if (
          !link ||
          !link.stripe_subscription_id ||
          !link.auto_renew ||
          link.status === 'canceled'
        ) {
          return null;
        }
        return (
          <Button
            size='small'
            type='danger'
            theme='light'
            onClick={() => cancelStripe(link, t, reload)}
          >
            {t('取消订阅')}
          </Button>
        );
      },
    },
  ];
}
