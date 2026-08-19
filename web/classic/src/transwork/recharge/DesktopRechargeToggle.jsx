import React, { useEffect, useState } from 'react';
import { Switch, Typography } from '@douyinfe/semi-ui';
import { API, showError, showSuccess, toBoolean } from '../../helpers';
import { useTranslation } from 'react-i18next';

const DESKTOP_RECHARGE_KEY = 'DesktopRechargeEnabled';

// Gate 2 for desktop recharge (see transwork/handler/recharge_availability.go).
// Mirrors the web/default toggle so the classic theme can flip the same option.
export default function DesktopRechargeToggle(props) {
  const { t } = useTranslation();
  const [enabled, setEnabled] = useState(false);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (props.options) {
      setEnabled(toBoolean(props.options[DESKTOP_RECHARGE_KEY]));
    }
  }, [props.options]);

  const handleChange = async (checked) => {
    setLoading(true);
    try {
      const res = await API.put('/api/option/', {
        key: DESKTOP_RECHARGE_KEY,
        value: checked ? 'true' : 'false',
      });
      if (res.data.success) {
        setEnabled(checked);
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
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'space-between',
        border: '1px solid var(--semi-color-border)',
        borderRadius: 8,
        padding: 16,
        marginTop: 16,
      }}
    >
      <div style={{ marginRight: 16 }}>
        <Typography.Text strong>{t('启用桌面充值')}</Typography.Text>
        <div>
          <Typography.Text type='tertiary'>
            {t('在 Gressio 桌面客户端显示充值按钮（需已配置支付渠道）。')}
          </Typography.Text>
        </div>
      </div>
      <Switch checked={enabled} loading={loading} onChange={handleChange} />
    </div>
  );
}
