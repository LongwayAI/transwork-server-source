import React, { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Card, Table, Button, Typography } from '@douyinfe/semi-ui';
import { IconRefresh } from '@douyinfe/semi-icons';
import { API, showError, timestamp2string } from '../../helpers';

// Gressio overlay (classic theme) — admin-only waitlist submissions dashboard.
// Reads the read-only admin endpoint GET /api/transwork/waitlist/admin (session
// AdminAuth) and renders the rows newest-first (backend already orders them) in
// a Semi Table. Kept self-contained under the transwork/ overlay per repo Rule 4;
// only the route + sidebar registration touch upstream files.
const { Title } = Typography;

const WaitlistPage = () => {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(false);
  const [rows, setRows] = useState([]);

  const loadWaitlist = async () => {
    setLoading(true);
    try {
      const res = await API.get('/api/transwork/waitlist/admin');
      if (res.data?.success) {
        setRows(res.data.data || []);
      } else {
        showError(res.data?.message || t('加载失败'));
      }
    } catch (e) {
      showError(t('加载失败'));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    loadWaitlist();
  }, []);

  const columns = [
    {
      title: t('姓名'),
      dataIndex: 'name',
    },
    {
      title: t('邮箱'),
      dataIndex: 'email',
    },
    {
      title: t('职位'),
      dataIndex: 'job',
    },
    {
      title: t('角色'),
      dataIndex: 'role',
    },
    {
      title: t('使用场景'),
      dataIndex: 'use_case',
    },
    {
      title: t('提交时间'),
      dataIndex: 'created_at',
      render: (value) => (value ? timestamp2string(value) : '-'),
    },
    {
      title: t('用户 ID'),
      dataIndex: 'user_id',
    },
  ];

  return (
    <div className='mt-[60px] px-2'>
      <Card
        title={<Title heading={4}>{t('候补名单')}</Title>}
        headerExtraContent={
          <Button
            icon={<IconRefresh />}
            loading={loading}
            onClick={loadWaitlist}
          >
            {t('刷新')}
          </Button>
        }
      >
        <Table
          columns={columns}
          dataSource={rows}
          loading={loading}
          rowKey='id'
          pagination={false}
        />
      </Card>
    </div>
  );
};

export default WaitlistPage;
