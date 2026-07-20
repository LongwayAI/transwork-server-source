import React, { useEffect, useState } from 'react';
import { Button, TextArea, Typography } from '@douyinfe/semi-ui';
import { API, showError, showSuccess, verifyJSON } from '../../helpers';
import { useTranslation } from 'react-i18next';

const RECHARGE_CONFIG_KEY = 'transwork_recharge.config';

const PLACEHOLDER = `{
  "credits_per_dollar": 100,
  "tiers": [
    { "usd": 10, "bonus_pct": 0 },
    { "usd": 30, "bonus_pct": 5 },
    { "usd": 100, "bonus_pct": 10 }
  ]
}`;

// Gressio desktop recharge tiers + standard exchange rate. Edits the
// transwork_recharge.config option (a JSON blob); empty => the server's built-in
// default (transwork/recharge_tiers.json) is used. The desktop client renders
// these tiers and the server grants the configured bonus credits, so the two
// always agree. Mirrors DesktopRechargeToggle's option round-trip.
export default function DesktopRechargeTiers(props) {
  const { t } = useTranslation();
  const [value, setValue] = useState('');
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (props.options) {
      setValue(props.options[RECHARGE_CONFIG_KEY] || '');
    }
  }, [props.options]);

  const handleSave = async () => {
    if (value.trim() !== '' && !verifyJSON(value)) {
      showError(t('桌面充值档位配置不是合法的 JSON'));
      return;
    }
    setLoading(true);
    try {
      const res = await API.put('/api/option/', {
        key: RECHARGE_CONFIG_KEY,
        value: value,
      });
      if (res.data.success) {
        showSuccess(t('更新成功'));
        props.refresh && props.refresh();
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
      <Typography.Text strong>{t('桌面充值档位')}</Typography.Text>
      <div style={{ marginTop: 4, marginBottom: 8 }}>
        <Typography.Text type='tertiary'>
          {t(
            'Gressio 桌面客户端的充值档位与赠送比例。留空则使用服务器内置默认值。credits_per_dollar 为每 1 美元对应的额度（标准汇率），bonus_pct 为额外赠送的百分比。',
          )}
        </Typography.Text>
      </div>
      <TextArea
        value={value}
        onChange={(v) => setValue(v)}
        autosize
        placeholder={PLACEHOLDER}
      />
      <Button onClick={handleSave} loading={loading} style={{ marginTop: 12 }}>
        {t('保存')}
      </Button>
    </div>
  );
}
