import { useTranslation } from 'react-i18next'
import { Switch } from '@/components/ui/switch'
import {
  getOptionValue,
  useSystemOptions,
} from '@/features/system-settings/hooks/use-system-options'
import { useUpdateOption } from '@/features/system-settings/hooks/use-update-option'

const DESKTOP_SUBSCRIPTION_KEY = 'DesktopSubscriptionEnabled'

export function DesktopSubscriptionToggle() {
  const { t } = useTranslation()
  const { data } = useSystemOptions()
  const updateOption = useUpdateOption()

  const enabled = getOptionValue(data?.data, {
    [DESKTOP_SUBSCRIPTION_KEY]: false,
  })[DESKTOP_SUBSCRIPTION_KEY]

  return (
    <div className='flex flex-row items-center justify-between rounded-lg border p-4'>
      <div className='space-y-0.5'>
        <p className='text-base font-medium'>
          {t('Enable desktop subscription')}
        </p>
        <p className='text-muted-foreground text-sm'>
          {t(
            'Show the Subscribe / Manage subscription options in the Gressio desktop client (requires a configured Stripe secret key).'
          )}
        </p>
      </div>
      <Switch
        checked={enabled}
        disabled={updateOption.isPending}
        onCheckedChange={(checked) =>
          updateOption.mutate({ key: DESKTOP_SUBSCRIPTION_KEY, value: checked })
        }
      />
    </div>
  )
}
