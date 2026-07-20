import React, { useEffect, useState } from 'react';
import { Button, InputNumber, Typography } from '@douyinfe/semi-ui';
import { API, showError, showSuccess } from '../../helpers';
import { useTranslation } from 'react-i18next';

const FREE_CREDIT_KEY = 'FreeCreditForNewUser';
const DEFAULT_FREE_CREDIT = 1000;

// One-time free starter credit (raw quota units) granted to a new user who
// continues without an invite code (requires InviteCodeOptional). Stored as a
// decimal string. Mirrors DesktopRechargeTiers' option round-trip.
export default function FreeCreditForNewUser(props) {
  const { t } = useTranslation();
  const [value, setValue] = useState(DEFAULT_FREE_CREDIT);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (props.options) {
      const raw = props.options[FREE_CREDIT_KEY];
      const parsed = parseInt(raw, 10);
      setValue(Number.isNaN(parsed) ? DEFAULT_FREE_CREDIT : parsed);
    }
  }, [props.options]);

  const handleSave = async () => {
    const amount =
      typeof value === 'number' && value >= 0 ? value : DEFAULT_FREE_CREDIT;
    setLoading(true);
    try {
      const res = await API.put('/api/option/', {
        key: FREE_CREDIT_KEY,
        value: String(amount),
      });
      if (res.data.success) {
        setValue(amount);
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
      <Typography.Text strong>{t('新用户免费额度')}</Typography.Text>
      <div style={{ marginTop: 4, marginBottom: 8 }}>
        <Typography.Text type='tertiary'>
          {t('用户不使用邀请码继续时一次性赠送的额度')}
        </Typography.Text>
      </div>
      <InputNumber
        value={value}
        onChange={(v) => setValue(v)}
        min={0}
        style={{ width: '100%' }}
      />
      <Button onClick={handleSave} loading={loading} style={{ marginTop: 12 }}>
        {t('保存')}
      </Button>
    </div>
  );
}
