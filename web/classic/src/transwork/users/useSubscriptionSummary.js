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
import { useState, useRef } from 'react';
import { API } from '../../helpers';

// Gressio overlay (classic theme) — fetches each visible user's primary
// in-period subscription for the admin user table. Kept here so the upstream
// useUsersData.jsx holds only a thin hook-call seam (repo Rule 4, overlay-first).
// Non-blocking: on failure the subscription columns simply render "—".
//
// The request-token ref guards against out-of-order responses: quick paging or
// searching can fire overlapping requests, and only the latest may apply its
// result, so a slower earlier response can't overwrite the current page.
export const useSubscriptionSummary = () => {
  const [subscriptionSummary, setSubscriptionSummary] = useState({});
  const requestRef = useRef(0);

  const loadSubscriptionSummary = async (userList) => {
    const ids = userList.map((u) => u.id);
    const requestId = ++requestRef.current;
    if (ids.length === 0) {
      setSubscriptionSummary({});
      return;
    }
    try {
      const res = await API.post('/api/subscription/admin/users/summary', {
        user_ids: ids,
      });
      if (requestId !== requestRef.current) return;
      const { success, data } = res.data;
      setSubscriptionSummary(success && data ? data : {});
    } catch (e) {
      if (requestId !== requestRef.current) return;
      setSubscriptionSummary({});
    }
  };

  return { subscriptionSummary, loadSubscriptionSummary };
};
