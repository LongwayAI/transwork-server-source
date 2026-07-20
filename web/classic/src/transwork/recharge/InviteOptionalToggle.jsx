import React, { useEffect, useState } from 'react';
import { Switch, Typography } from '@douyinfe/semi-ui';
import { API, showError, showSuccess, toBoolean } from '../../helpers';
import { useTranslation } from 'react-i18next';

const INVITE_CODE_OPTIONAL_KEY = 'InviteCodeOptional';

// When enabled, new desktop users may skip the invite code and receive free
// starter credit (see FreeCreditForNewUser). Mirrors DesktopRechargeToggle's
// option round-trip.
export default function InviteOptionalToggle(props) {
  const { t } = useTranslation();
  const [enabled, setEnabled] = useState(false);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (props.options) {
      setEnabled(toBoolean(props.options[INVITE_CODE_OPTIONAL_KEY]));
    }
  }, [props.options]);

  const handleChange = async (checked) => {
    setLoading(true);
    try {
      const res = await API.put('/api/option/', {
        key: INVITE_CODE_OPTIONAL_KEY,
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
        <Typography.Text strong>{t('邀请码可选')}</Typography.Text>
        <div>
          <Typography.Text type='tertiary'>
            {t('开启后，新用户无需邀请码即可继续使用并获得免费初始额度')}
          </Typography.Text>
        </div>
      </div>
      <Switch checked={enabled} loading={loading} onChange={handleChange} />
    </div>
  );
}
