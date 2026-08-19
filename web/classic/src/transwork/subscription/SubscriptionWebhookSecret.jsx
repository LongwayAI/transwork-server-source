import React, { useState } from 'react';
import { Button, Input, Typography } from '@douyinfe/semi-ui';
import {
  API,
  removeTrailingSlash,
  showError,
  showSuccess,
} from '../../helpers';
import { useTranslation } from 'react-i18next';

const WEBHOOK_SECRET_KEY = 'transwork_subscription.stripe_webhook_secret';

// Signing secret for the recurring-subscription webhook ("endpoint B",
// transwork/handler/stripe_subscription_webhook.go), which is a SEPARATE Stripe
// endpoint from the upstream top-up one above and therefore has its own secret.
// The handler is fail-closed: while this is empty every delivery is rejected 503.
//
// Write-only by necessity, not by choice: controller.GetOptions drops any key
// ending in "secret", so the current value can never be read back and the field
// always renders empty. An empty save is refused rather than silently clearing a
// configured secret and taking the endpoint down.
export default function SubscriptionWebhookSecret(props) {
  const { t } = useTranslation();
  const [value, setValue] = useState('');
  const [loading, setLoading] = useState(false);

  const serverAddress = props.options?.ServerAddress
    ? removeTrailingSlash(props.options.ServerAddress)
    : t('网站地址');

  const handleSave = async () => {
    const secret = value.trim();
    if (secret === '') {
      showError(t('请填写订阅 Webhook 签名密钥'));
      return;
    }
    setLoading(true);
    try {
      const res = await API.put('/api/option/', {
        key: WEBHOOK_SECRET_KEY,
        value: secret,
      });
      if (res.data.success) {
        setValue('');
        showSuccess(t('更新成功'));
        // Deliberately NOT calling props.refresh(): the parent resets its whole
        // Stripe form from props.options on every change, so refreshing here
        // would discard the operator's unsaved edits to the other fields -- and
        // GetOptions strips secrets, so the two secret inputs would come back
        // blank. Nothing this card displays changes as a result of this save.
      } else {
        showError(res.data.message || t('更新失败'));
      }
    } catch (error) {
      showError(t('更新失败'));
    }
    setLoading(false);
  };

  return (
    <div
      style={{
        border: '1px solid var(--semi-color-border)',
        borderRadius: 8,
        padding: 16,
        marginTop: 16,
      }}
    >
      <Typography.Text strong>{t('订阅 Webhook 签名密钥')}</Typography.Text>
      <div style={{ marginTop: 4, marginBottom: 8 }}>
        <Typography.Text type='tertiary'>
          {t(
            '订阅续费回调使用独立的 Stripe Webhook 端点，密钥与上方充值 Webhook 互不相通。保存后不会回显，留空保存无效。',
          )}
        </Typography.Text>
        <div style={{ marginTop: 4 }}>
          <Typography.Text type='tertiary'>
            {t('回调地址')}：{serverAddress}
            /transwork/stripe/subscription-webhook
          </Typography.Text>
        </div>
        <div style={{ marginTop: 4 }}>
          <Typography.Text type='tertiary'>
            {t('需要包含事件')}：checkout.session.completed、
            invoice.payment_succeeded、invoice.payment_failed、
            customer.subscription.updated、customer.subscription.deleted
          </Typography.Text>
        </div>
      </div>
      <Input
        value={value}
        onChange={(v) => setValue(v)}
        mode='password'
        onEnterPress={handleSave}
        aria-label={t('订阅 Webhook 签名密钥')}
        placeholder={t('例如 whsec_xxx')}
      />
      <Button onClick={handleSave} loading={loading} style={{ marginTop: 12 }}>
        {t('保存')}
      </Button>
    </div>
  );
}
